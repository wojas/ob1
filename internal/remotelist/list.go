package remotelist

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"path"
	"sort"
	"strings"

	"github.com/gorilla/websocket"

	"github.com/wojas/ob1/internal/vaultcrypto"
	"github.com/wojas/ob1/internal/vaultstore"
)

type Entry struct {
	Path   string
	UID    int64
	Size   int64
	CTime  int64
	MTime  int64
	Hash   string
	Device string
	Folder bool
}

type File struct {
	Entry Entry
	Body  []byte
}

type initMessage struct {
	Op                string `json:"op"`
	Token             string `json:"token"`
	ID                string `json:"id"`
	KeyHash           string `json:"keyhash"`
	Version           int64  `json:"version"`
	Initial           bool   `json:"initial"`
	Device            string `json:"device"`
	EncryptionVersion int    `json:"encryption_version"`
}

type serverMessage struct {
	Op     string `json:"op"`
	Res    string `json:"res"`
	Status string `json:"status"`
	Msg    string `json:"msg"`
}

type pushMessage struct {
	Op      string `json:"op"`
	UID     int64  `json:"uid"`
	Path    string `json:"path"`
	Size    int64  `json:"size"`
	Hash    string `json:"hash"`
	CTime   int64  `json:"ctime"`
	MTime   int64  `json:"mtime"`
	Device  string `json:"device"`
	Folder  bool   `json:"folder"`
	Deleted bool   `json:"deleted"`
}

type pushRequest struct {
	Op          string  `json:"op"`
	Path        string  `json:"path"`
	RelatedPath *string `json:"relatedpath"`
	Extension   string  `json:"extension"`
	Hash        string  `json:"hash"`
	CTime       int64   `json:"ctime"`
	MTime       int64   `json:"mtime"`
	Folder      bool    `json:"folder"`
	Deleted     bool    `json:"deleted"`
	Size        int64   `json:"size,omitempty"`
	Pieces      int     `json:"pieces,omitempty"`
}

type Upload struct {
	Path  string
	Body  []byte
	MTime int64
}

type pullRequest struct {
	Op  string `json:"op"`
	UID int64  `json:"uid"`
}

type pullResponse struct {
	Deleted bool  `json:"deleted"`
	Size    int64 `json:"size"`
	Pieces  int   `json:"pieces"`
}

func Snapshot(ctx context.Context, logger *slog.Logger, token string, state vaultstore.VaultState) ([]Entry, error) {
	if strings.TrimSpace(token) == "" {
		return nil, errors.New("missing auth token")
	}

	rawKey, conn, err := dialAndInit(ctx, logger, token, state)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	return snapshotEntries(logger, conn, rawKey, state)
}

func ReadFile(ctx context.Context, logger *slog.Logger, token string, state vaultstore.VaultState, targetPath string) ([]byte, error) {
	files, err := ReadFiles(ctx, logger, token, state, []string{targetPath})
	if err != nil {
		return nil, err
	}
	if len(files) != 1 {
		return nil, fmt.Errorf("remote file %q not found", targetPath)
	}

	return files[0].Body, nil
}

func ReadFiles(ctx context.Context, logger *slog.Logger, token string, state vaultstore.VaultState, targetPaths []string) ([]File, error) {
	if strings.TrimSpace(token) == "" {
		return nil, errors.New("missing auth token")
	}

	rawKey, conn, err := dialAndInit(ctx, logger, token, state)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	entries, err := snapshotEntries(logger, conn, rawKey, state)
	if err != nil {
		return nil, err
	}

	entriesByPath := make(map[string]Entry, len(entries))
	for _, entry := range entries {
		entriesByPath[entry.Path] = entry
	}

	files := make([]File, 0, len(targetPaths))
	for _, targetPath := range targetPaths {
		entry, ok := entriesByPath[targetPath]
		if !ok {
			return nil, fmt.Errorf("remote file %q not found", targetPath)
		}
		if entry.Folder {
			return nil, fmt.Errorf("%q is a folder", targetPath)
		}

		body, err := pullFile(logger, conn, rawKey, state, targetPath, entry.UID)
		if err != nil {
			return nil, err
		}

		files = append(files, File{
			Entry: entry,
			Body:  body,
		})
	}

	return files, nil
}

func pullFile(logger *slog.Logger, conn *websocket.Conn, rawKey []byte, state vaultstore.VaultState, targetPath string, uid int64) ([]byte, error) {
	if err := writeJSONMessage(logger, conn, pullRequest{
		Op:  "pull",
		UID: uid,
	}); err != nil {
		return nil, err
	}

	responseBody, err := readCommandResponse(logger, conn)
	if err != nil {
		return nil, err
	}

	var response pullResponse
	if err := json.Unmarshal(responseBody, &response); err != nil {
		return nil, fmt.Errorf("decode pull response: %w", err)
	}
	if response.Deleted {
		return nil, fmt.Errorf("remote file %q was deleted", targetPath)
	}

	bufferCap := 0
	if response.Size > 0 {
		bufferCap = int(response.Size)
	}
	encrypted := make([]byte, 0, bufferCap)
	for i := 0; i < response.Pieces; i++ {
		messageType, chunk, err := conn.ReadMessage()
		if err != nil {
			return nil, fmt.Errorf("read pull chunk: %w", err)
		}

		switch messageType {
		case websocket.BinaryMessage:
			encrypted = append(encrypted, chunk...)
		case websocket.TextMessage:
			logger.Debug("websocket recv", "payload", prettyJSON(chunk))

			var msg serverMessage
			if err := json.Unmarshal(chunk, &msg); err != nil {
				return nil, fmt.Errorf("decode unexpected text frame: %w", err)
			}
			if msg.Op == "pong" {
				i--
				continue
			}
			if msg.Status == "err" || msg.Res == "err" {
				return nil, errors.New(strings.TrimSpace(msg.Msg))
			}
			return nil, fmt.Errorf("unexpected text frame during pull: %s", strings.TrimSpace(string(chunk)))
		default:
			return nil, fmt.Errorf("unexpected websocket message type %d during pull", messageType)
		}
	}

	if response.Size > 0 && int64(len(encrypted)) != response.Size {
		logger.Warn("pull size mismatch", "expected", response.Size, "actual", len(encrypted))
	}

	return vaultcrypto.DecodeFileBody(rawKey, state.EncryptionVersion, encrypted)
}

func dialAndInit(ctx context.Context, logger *slog.Logger, token string, state vaultstore.VaultState) ([]byte, *websocket.Conn, error) {
	rawKey, err := vaultcrypto.DecodeKey(state.EncryptionKey)
	if err != nil {
		return nil, nil, err
	}

	wsURL, err := websocketURL(state.Host)
	if err != nil {
		return nil, nil, err
	}

	logger.Debug("websocket connect", "url", wsURL)
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, wsURL, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("connect websocket: %w", err)
	}

	req := initMessage{
		Op:                "init",
		Token:             token,
		ID:                state.VaultID,
		KeyHash:           state.KeyHash,
		Version:           0,
		Initial:           true,
		Device:            state.DeviceName,
		EncryptionVersion: state.EncryptionVersion,
	}
	if err := writeJSONMessage(logger, conn, req); err != nil {
		conn.Close()
		return nil, nil, err
	}

	if err := awaitInitOK(logger, conn); err != nil {
		conn.Close()
		return nil, nil, err
	}

	return rawKey, conn, nil
}

func snapshotEntries(logger *slog.Logger, conn *websocket.Conn, rawKey []byte, state vaultstore.VaultState) ([]Entry, error) {
	entriesByUID := make(map[int64]Entry)
	for {
		payload, err := readJSONMessage(logger, conn)
		if err != nil {
			return nil, err
		}

		var msg serverMessage
		if err := json.Unmarshal(payload, &msg); err != nil {
			return nil, fmt.Errorf("decode websocket message: %w", err)
		}

		switch msg.Op {
		case "pong":
			continue
		case "ready":
			entries := make([]Entry, 0, len(entriesByUID))
			for _, entry := range entriesByUID {
				entries = append(entries, entry)
			}
			sort.Slice(entries, func(i, j int) bool {
				if entries[i].Path == entries[j].Path {
					return entries[i].UID < entries[j].UID
				}
				return entries[i].Path < entries[j].Path
			})
			return entries, nil
		case "push":
			var push pushMessage
			if err := json.Unmarshal(payload, &push); err != nil {
				return nil, fmt.Errorf("decode push message: %w", err)
			}

			if push.Deleted {
				delete(entriesByUID, push.UID)
				continue
			}

			path, err := vaultcrypto.DecodeMetadata(rawKey, state.Salt, state.EncryptionVersion, push.Path)
			if err != nil {
				return nil, fmt.Errorf("decode remote path for uid %d: %w", push.UID, err)
			}

			hash := ""
			if strings.TrimSpace(push.Hash) != "" {
				hash, err = vaultcrypto.DecodeMetadata(rawKey, state.Salt, state.EncryptionVersion, push.Hash)
				if err != nil {
					return nil, fmt.Errorf("decode remote hash for uid %d: %w", push.UID, err)
				}
			}

			entriesByUID[push.UID] = Entry{
				Path:   path,
				UID:    push.UID,
				Size:   push.Size,
				Hash:   hash,
				CTime:  push.CTime,
				MTime:  push.MTime,
				Device: push.Device,
				Folder: push.Folder,
			}
		default:
			if msg.Status == "err" || msg.Res == "err" {
				return nil, errors.New(strings.TrimSpace(msg.Msg))
			}
		}
	}
}

func awaitInitOK(logger *slog.Logger, conn *websocket.Conn) error {
	for {
		payload, err := readJSONMessage(logger, conn)
		if err != nil {
			return err
		}

		var msg serverMessage
		if err := json.Unmarshal(payload, &msg); err != nil {
			return fmt.Errorf("decode init response: %w", err)
		}

		if msg.Op == "pong" {
			continue
		}

		if msg.Res == "ok" {
			return nil
		}

		if msg.Status == "err" || msg.Res == "err" {
			if strings.TrimSpace(msg.Msg) == "" {
				return errors.New("websocket init failed")
			}
			return errors.New(strings.TrimSpace(msg.Msg))
		}

		return fmt.Errorf("unexpected websocket init response: %s", strings.TrimSpace(string(payload)))
	}
}

func writeJSONMessage(logger *slog.Logger, conn *websocket.Conn, message any) error {
	body, err := json.MarshalIndent(message, "", "  ")
	if err != nil {
		return fmt.Errorf("encode websocket message: %w", err)
	}

	logger.Debug("websocket send", "payload", string(body))
	if err := conn.WriteMessage(websocket.TextMessage, body); err != nil {
		return fmt.Errorf("send websocket message: %w", err)
	}

	return nil
}

func readJSONMessage(logger *slog.Logger, conn *websocket.Conn) ([]byte, error) {
	messageType, body, err := conn.ReadMessage()
	if err != nil {
		return nil, fmt.Errorf("read websocket message: %w", err)
	}

	if messageType != websocket.TextMessage {
		return nil, fmt.Errorf("unexpected websocket message type %d", messageType)
	}

	logger.Debug("websocket recv", "payload", prettyJSON(body))

	return body, nil
}

func readCommandResponse(logger *slog.Logger, conn *websocket.Conn) ([]byte, error) {
	for {
		body, err := readJSONMessage(logger, conn)
		if err != nil {
			return nil, err
		}

		var msg serverMessage
		if err := json.Unmarshal(body, &msg); err != nil {
			return body, nil
		}

		switch msg.Op {
		case "pong", "push", "ready":
			logger.Debug("skipping event while awaiting command response", "op", msg.Op)
			continue
		default:
			return body, nil
		}
	}
}

func websocketURL(host string) (string, error) {
	trimmed := strings.TrimSpace(host)
	if trimmed == "" {
		return "", errors.New("missing vault host")
	}

	if strings.HasPrefix(trimmed, "ws://") || strings.HasPrefix(trimmed, "wss://") {
		return trimmed, nil
	}

	u := url.URL{Host: trimmed}
	lower := strings.ToLower(trimmed)
	switch {
	case strings.HasPrefix(lower, "localhost"), strings.HasPrefix(lower, "127.0.0.1"):
		u.Scheme = "ws"
	default:
		u.Scheme = "wss"
	}

	return u.String(), nil
}

func prettyJSON(body []byte) string {
	var value any
	if err := json.Unmarshal(body, &value); err != nil {
		return string(body)
	}

	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return string(body)
	}

	return string(encoded)
}

func PutFiles(ctx context.Context, logger *slog.Logger, token string, state vaultstore.VaultState, uploads []Upload) error {
	if strings.TrimSpace(token) == "" {
		return errors.New("missing auth token")
	}

	rawKey, conn, err := dialAndInit(ctx, logger, token, state)
	if err != nil {
		return err
	}
	defer conn.Close()

	entries, err := snapshotEntries(logger, conn, rawKey, state)
	if err != nil {
		return err
	}

	entriesByPath := make(map[string]Entry, len(entries))
	for _, entry := range entries {
		entriesByPath[entry.Path] = entry
	}

	if err := ensureRemoteDirectories(logger, conn, rawKey, state, entriesByPath, uploads); err != nil {
		return err
	}

	for _, upload := range uploads {
		if err := putSingleFile(logger, conn, rawKey, state, entriesByPath, upload); err != nil {
			return err
		}
	}

	return nil
}

func ensureRemoteDirectories(logger *slog.Logger, conn *websocket.Conn, rawKey []byte, state vaultstore.VaultState, entriesByPath map[string]Entry, uploads []Upload) error {
	required := make(map[string]struct{})
	for _, upload := range uploads {
		dir := path.Dir(upload.Path)
		for dir != "." && dir != "/" {
			required[dir] = struct{}{}
			dir = path.Dir(dir)
		}
	}

	dirs := make([]string, 0, len(required))
	for dir := range required {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)

	for _, dir := range dirs {
		if existing, ok := entriesByPath[dir]; ok {
			if !existing.Folder {
				return fmt.Errorf("remote path %q exists as a file", dir)
			}
			continue
		}

		encodedPath, err := vaultcrypto.EncodeMetadata(rawKey, state.Salt, state.EncryptionVersion, dir)
		if err != nil {
			return fmt.Errorf("encode folder path %q: %w", dir, err)
		}

		if err := sendPush(logger, conn, pushRequest{
			Op:        "push",
			Path:      encodedPath,
			Extension: "",
			Hash:      "",
			CTime:     0,
			MTime:     0,
			Folder:    true,
			Deleted:   false,
		}, nil); err != nil {
			return fmt.Errorf("create remote folder %q: %w", dir, err)
		}

		entriesByPath[dir] = Entry{Path: dir, Folder: true}
	}

	return nil
}

func putSingleFile(logger *slog.Logger, conn *websocket.Conn, rawKey []byte, state vaultstore.VaultState, entriesByPath map[string]Entry, upload Upload) error {
	existing, exists := entriesByPath[upload.Path]
	if exists && existing.Folder {
		return fmt.Errorf("remote path %q exists as a folder", upload.Path)
	}

	hashHex := vaultcrypto.PlaintextHash(upload.Body)
	if exists && existing.Hash == hashHex {
		logger.Info("skipping unchanged file", "path", upload.Path)
		return nil
	}

	encodedPath, err := vaultcrypto.EncodeMetadata(rawKey, state.Salt, state.EncryptionVersion, upload.Path)
	if err != nil {
		return fmt.Errorf("encode file path %q: %w", upload.Path, err)
	}

	encodedHash, err := vaultcrypto.EncodeMetadata(rawKey, state.Salt, state.EncryptionVersion, hashHex)
	if err != nil {
		return fmt.Errorf("encode file hash %q: %w", upload.Path, err)
	}

	encryptedBody, err := vaultcrypto.EncodeFileBody(rawKey, state.EncryptionVersion, upload.Body)
	if err != nil {
		return fmt.Errorf("encrypt file body %q: %w", upload.Path, err)
	}

	ctime := upload.MTime
	if exists && existing.CTime > 0 {
		ctime = existing.CTime
	}

	extension := strings.TrimPrefix(path.Ext(upload.Path), ".")
	const pieceSize = 2 * 1024 * 1024
	pieces := 0
	if len(encryptedBody) > 0 {
		pieces = (len(encryptedBody) + pieceSize - 1) / pieceSize
	}

	if err := sendPush(logger, conn, pushRequest{
		Op:        "push",
		Path:      encodedPath,
		Extension: extension,
		Hash:      encodedHash,
		CTime:     ctime,
		MTime:     upload.MTime,
		Folder:    false,
		Deleted:   false,
		Size:      int64(len(encryptedBody)),
		Pieces:    pieces,
	}, encryptedBody); err != nil {
		return fmt.Errorf("upload %q: %w", upload.Path, err)
	}

	entriesByPath[upload.Path] = Entry{
		Path:   upload.Path,
		Size:   int64(len(upload.Body)),
		Hash:   hashHex,
		CTime:  ctime,
		MTime:  upload.MTime,
		Folder: false,
	}
	logger.Info("uploaded file", "path", upload.Path, "bytes", len(upload.Body))

	return nil
}

func sendPush(logger *slog.Logger, conn *websocket.Conn, req pushRequest, encryptedBody []byte) error {
	if err := writeJSONMessage(logger, conn, req); err != nil {
		return err
	}

	responseBody, err := readCommandResponse(logger, conn)
	if err != nil {
		return err
	}

	var response serverMessage
	if err := json.Unmarshal(responseBody, &response); err != nil {
		return fmt.Errorf("decode push response: %w", err)
	}
	if response.Status == "err" || response.Res == "err" {
		if strings.TrimSpace(response.Msg) == "" {
			return errors.New("push failed")
		}
		return errors.New(strings.TrimSpace(response.Msg))
	}

	if response.Res == "ok" || len(encryptedBody) == 0 {
		return nil
	}

	const pieceSize = 2 * 1024 * 1024
	for start := 0; start < len(encryptedBody); start += pieceSize {
		end := start + pieceSize
		if end > len(encryptedBody) {
			end = len(encryptedBody)
		}

		chunk := encryptedBody[start:end]
		logger.Debug("websocket send binary", "bytes", len(chunk))
		if err := conn.WriteMessage(websocket.BinaryMessage, chunk); err != nil {
			return fmt.Errorf("send push chunk: %w", err)
		}

		ackBody, err := readCommandResponse(logger, conn)
		if err != nil {
			return err
		}

		var ack serverMessage
		if err := json.Unmarshal(ackBody, &ack); err != nil {
			return fmt.Errorf("decode push chunk response: %w", err)
		}
		if ack.Status == "err" || ack.Res == "err" {
			if strings.TrimSpace(ack.Msg) == "" {
				return errors.New("push chunk failed")
			}
			return errors.New(strings.TrimSpace(ack.Msg))
		}
	}

	return nil
}
