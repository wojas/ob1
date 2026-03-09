package testserver

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/wojas/ob1/internal/obsidianapi"
	"github.com/wojas/ob1/internal/vaultcrypto"
)

const (
	defaultEmail         = "test@example.com"
	defaultPassword      = "account-password"
	defaultToken         = "test-token"
	defaultVaultPassword = "vault-password"
	defaultVaultID       = "vault-1"
	defaultVaultName     = "Test Vault"
	defaultVaultSalt     = "test-vault-salt"
	defaultDevice        = "test-server"
)

type Options struct {
	Email         string
	Password      string
	Token         string
	VaultPassword string
	VaultID       string
	VaultName     string
	VaultSalt     string
	InitialFiles  map[string][]byte
}

type Server struct {
	server *httptest.Server

	email         string
	password      string
	token         string
	vaultPassword string
	rawKey        []byte
	keyHash       string
	vault         obsidianapi.Vault
	userInfo      map[string]any

	mu          sync.Mutex
	nextUID     int64
	version     int64
	signOuts    int
	entriesByID map[int64]*entry
	entriesBy   map[string]*entry
}

type entry struct {
	UID    int64
	Path   string
	Folder bool
	Body   []byte
	CTime  int64
	MTime  int64
	Device string
}

type pushEnvelope struct {
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

type pullRequest struct {
	Op  string `json:"op"`
	UID int64  `json:"uid"`
}

type pushRequest struct {
	Op        string `json:"op"`
	Path      string `json:"path"`
	Hash      string `json:"hash"`
	CTime     int64  `json:"ctime"`
	MTime     int64  `json:"mtime"`
	Folder    bool   `json:"folder"`
	Deleted   bool   `json:"deleted"`
	Size      int64  `json:"size"`
	Pieces    int    `json:"pieces"`
	Extension string `json:"extension"`
}

type pendingUpload struct {
	path   string
	ctime  int64
	mtime  int64
	pieces int
	hash   string
	buffer bytes.Buffer
}

type sessionRequest struct {
	Token string `json:"token"`
}

type signInRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
	MFA      string `json:"mfa"`
}

type vaultListRequest struct {
	Token string `json:"token"`
}

type vaultAccessRequest struct {
	Token             string `json:"token"`
	VaultUID          string `json:"vault_uid"`
	KeyHash           string `json:"keyhash"`
	Host              string `json:"host"`
	EncryptionVersion int    `json:"encryption_version"`
}

type initRequest struct {
	Op                string `json:"op"`
	Token             string `json:"token"`
	ID                string `json:"id"`
	KeyHash           string `json:"keyhash"`
	Version           int64  `json:"version"`
	Initial           bool   `json:"initial"`
	Device            string `json:"device"`
	EncryptionVersion int    `json:"encryption_version"`
}

func New(opts Options) (*Server, error) {
	s := &Server{
		email:         withDefault(opts.Email, defaultEmail),
		password:      withDefault(opts.Password, defaultPassword),
		token:         withDefault(opts.Token, defaultToken),
		vaultPassword: withDefault(opts.VaultPassword, defaultVaultPassword),
		nextUID:       100,
		entriesByID:   make(map[int64]*entry),
		entriesBy:     make(map[string]*entry),
	}

	rawKey, err := vaultcrypto.DeriveKey(s.vaultPassword, withDefault(opts.VaultSalt, defaultVaultSalt))
	if err != nil {
		return nil, err
	}
	keyHash, err := vaultcrypto.KeyHash(rawKey, withDefault(opts.VaultSalt, defaultVaultSalt), obsidianapi.SupportedEncryptionVersion)
	if err != nil {
		return nil, err
	}

	s.rawKey = rawKey
	s.keyHash = keyHash
	s.vault = obsidianapi.Vault{
		ID:                withDefault(opts.VaultID, defaultVaultID),
		Name:              withDefault(opts.VaultName, defaultVaultName),
		Region:            "test",
		Salt:              withDefault(opts.VaultSalt, defaultVaultSalt),
		EncryptionVersion: obsidianapi.SupportedEncryptionVersion,
	}
	s.userInfo = map[string]any{
		"name":  "Test User",
		"email": s.email,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/user/signin", s.handleSignIn)
	mux.HandleFunc("/user/signout", s.handleSignOut)
	mux.HandleFunc("/user/info", s.handleUserInfo)
	mux.HandleFunc("/vault/list", s.handleVaultList)
	mux.HandleFunc("/vault/access", s.handleVaultAccess)
	mux.HandleFunc("/ws", s.handleWebsocket)

	s.server = httptest.NewServer(mux)
	s.vault.Host = strings.Replace(s.server.URL, "http://", "ws://", 1) + "/ws"

	for filePath, body := range opts.InitialFiles {
		if err := s.putSeedFile(filePath, body); err != nil {
			s.server.Close()
			return nil, err
		}
	}

	return s, nil
}

func (s *Server) Close() {
	if s == nil || s.server == nil {
		return
	}

	s.server.Close()
}

func (s *Server) APIBaseURL() string {
	return s.server.URL
}

func (s *Server) Email() string {
	return s.email
}

func (s *Server) AccountPassword() string {
	return s.password
}

func (s *Server) VaultPassword() string {
	return s.vaultPassword
}

func (s *Server) VaultID() string {
	return s.vault.ID
}

func (s *Server) SignOutCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.signOuts
}

func (s *Server) FileBody(filePath string) ([]byte, bool) {
	normalized := normalizePath(filePath)
	if normalized == "" {
		return nil, false
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	current, ok := s.entriesBy[normalized]
	if !ok || current.Folder {
		return nil, false
	}

	return append([]byte(nil), current.Body...), true
}

func (s *Server) SetFileBody(filePath string, body []byte) error {
	now := time.Now().UTC().UnixMilli()
	return s.putFile(filePath, body, now, now)
}

func (s *Server) handleSignIn(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodOptions:
		w.WriteHeader(http.StatusNoContent)
		return
	case http.MethodPost:
	default:
		http.NotFound(w, r)
		return
	}

	var req signInRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Email != s.email || req.Password != s.password {
		writeError(w, http.StatusUnauthorized, "invalid credentials")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"token": s.token,
		"user":  s.userInfo,
	})
}

func (s *Server) handleSignOut(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}

	var req sessionRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Token != s.token {
		writeError(w, http.StatusUnauthorized, "invalid token")
		return
	}

	s.mu.Lock()
	s.signOuts++
	s.mu.Unlock()

	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleUserInfo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}

	var req sessionRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Token != s.token {
		writeError(w, http.StatusUnauthorized, "invalid token")
		return
	}

	writeJSON(w, http.StatusOK, s.userInfo)
}

func (s *Server) handleVaultList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}

	var req vaultListRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Token != s.token {
		writeError(w, http.StatusUnauthorized, "invalid token")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"vaults": []obsidianapi.Vault{s.vault},
		"shared": []obsidianapi.Vault{},
	})
}

func (s *Server) handleVaultAccess(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}

	var req vaultAccessRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	switch {
	case req.Token != s.token:
		writeError(w, http.StatusUnauthorized, "invalid token")
	case req.VaultUID != s.vault.ID:
		writeError(w, http.StatusBadRequest, "unknown vault")
	case req.KeyHash != s.keyHash:
		writeError(w, http.StatusUnauthorized, "invalid keyhash")
	case req.Host != s.vault.Host:
		writeError(w, http.StatusBadRequest, "unexpected host")
	case req.EncryptionVersion != s.vault.EncryptionVersion:
		writeError(w, http.StatusBadRequest, "unsupported encryption version")
	default:
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

func (s *Server) handleWebsocket(w http.ResponseWriter, r *http.Request) {
	upgrader := websocket.Upgrader{
		CheckOrigin: func(_ *http.Request) bool {
			return true
		},
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	_, body, err := conn.ReadMessage()
	if err != nil {
		return
	}

	var initReq initRequest
	if err := json.Unmarshal(body, &initReq); err != nil {
		_ = writeWSJSON(conn, map[string]any{"res": "err", "msg": "invalid init"})
		return
	}
	switch {
	case initReq.Op != "init":
		_ = writeWSJSON(conn, map[string]any{"res": "err", "msg": "missing init"})
		return
	case initReq.Token != s.token:
		_ = writeWSJSON(conn, map[string]any{"res": "err", "msg": "invalid token"})
		return
	case initReq.ID != s.vault.ID:
		_ = writeWSJSON(conn, map[string]any{"res": "err", "msg": "unknown vault"})
		return
	case initReq.KeyHash != s.keyHash:
		_ = writeWSJSON(conn, map[string]any{"res": "err", "msg": "invalid keyhash"})
		return
	case initReq.EncryptionVersion != s.vault.EncryptionVersion:
		_ = writeWSJSON(conn, map[string]any{"res": "err", "msg": "unsupported encryption version"})
		return
	}

	if err := writeWSJSON(conn, map[string]any{"res": "ok"}); err != nil {
		return
	}

	if err := s.writeSnapshot(conn); err != nil {
		return
	}

	var pending *pendingUpload
	for {
		messageType, message, err := conn.ReadMessage()
		if err != nil {
			return
		}

		switch messageType {
		case websocket.TextMessage:
			var envelope struct {
				Op string `json:"op"`
			}
			if err := json.Unmarshal(message, &envelope); err != nil {
				_ = writeWSJSON(conn, map[string]any{"res": "err", "msg": "invalid request"})
				return
			}

			switch envelope.Op {
			case "pull":
				var req pullRequest
				if err := json.Unmarshal(message, &req); err != nil {
					_ = writeWSJSON(conn, map[string]any{"res": "err", "msg": "invalid pull"})
					return
				}
				if err := s.handlePull(conn, req); err != nil {
					_ = writeWSJSON(conn, map[string]any{"res": "err", "msg": err.Error()})
					return
				}
			case "push":
				var req pushRequest
				if err := json.Unmarshal(message, &req); err != nil {
					_ = writeWSJSON(conn, map[string]any{"res": "err", "msg": "invalid push"})
					return
				}
				nextPending, err := s.handlePushStart(conn, req)
				if err != nil {
					_ = writeWSJSON(conn, map[string]any{"res": "err", "msg": err.Error()})
					return
				}
				pending = nextPending
			default:
				_ = writeWSJSON(conn, map[string]any{"res": "err", "msg": "unsupported operation"})
				return
			}
		case websocket.BinaryMessage:
			if pending == nil {
				_ = writeWSJSON(conn, map[string]any{"res": "err", "msg": "unexpected binary frame"})
				return
			}

			pending.buffer.Write(message)
			pending.pieces--
			if pending.pieces == 0 {
				if err := s.finishUpload(*pending); err != nil {
					_ = writeWSJSON(conn, map[string]any{"res": "err", "msg": err.Error()})
					return
				}
				pending = nil
			}

			if err := writeWSJSON(conn, map[string]any{"res": "ok"}); err != nil {
				return
			}
		default:
			_ = writeWSJSON(conn, map[string]any{"res": "err", "msg": "unsupported frame"})
			return
		}
	}
}

func (s *Server) handlePull(conn *websocket.Conn, req pullRequest) error {
	s.mu.Lock()
	current, ok := s.entriesByID[req.UID]
	if !ok || current.Folder {
		s.mu.Unlock()
		return errors.New("unknown file")
	}

	entryCopy := *current
	s.mu.Unlock()

	encrypted, err := vaultcrypto.EncodeFileBody(s.rawKey, s.vault.EncryptionVersion, entryCopy.Body)
	if err != nil {
		return err
	}

	if err := writeWSJSON(conn, map[string]any{
		"deleted": false,
		"size":    len(encrypted),
		"pieces":  pieceCount(len(encrypted)),
	}); err != nil {
		return err
	}

	if len(encrypted) == 0 {
		return nil
	}

	const pieceSize = 2 * 1024 * 1024
	for start := 0; start < len(encrypted); start += pieceSize {
		end := start + pieceSize
		if end > len(encrypted) {
			end = len(encrypted)
		}
		if err := conn.WriteMessage(websocket.BinaryMessage, encrypted[start:end]); err != nil {
			return err
		}
	}

	return nil
}

func (s *Server) handlePushStart(conn *websocket.Conn, req pushRequest) (*pendingUpload, error) {
	decodedPath, err := vaultcrypto.DecodeMetadata(s.rawKey, s.vault.Salt, s.vault.EncryptionVersion, req.Path)
	if err != nil {
		return nil, fmt.Errorf("decode path: %w", err)
	}

	normalized := normalizePath(decodedPath)
	if normalized == "" {
		return nil, errors.New("invalid path")
	}

	if req.Deleted {
		s.deletePath(normalized)
		return nil, writeWSJSON(conn, map[string]any{"res": "ok"})
	}

	if req.Folder {
		if err := s.ensureFolder(normalized); err != nil {
			return nil, err
		}
		return nil, writeWSJSON(conn, map[string]any{"res": "ok"})
	}

	decodedHash := ""
	if strings.TrimSpace(req.Hash) != "" {
		decodedHash, err = vaultcrypto.DecodeMetadata(s.rawKey, s.vault.Salt, s.vault.EncryptionVersion, req.Hash)
		if err != nil {
			return nil, fmt.Errorf("decode hash: %w", err)
		}
	}

	if req.Pieces == 0 {
		if err := s.finishUpload(pendingUpload{
			path:   normalized,
			ctime:  req.CTime,
			mtime:  req.MTime,
			pieces: 0,
			hash:   decodedHash,
		}); err != nil {
			return nil, err
		}
		return nil, writeWSJSON(conn, map[string]any{"res": "ok"})
	}

	if err := writeWSJSON(conn, map[string]any{"res": "continue"}); err != nil {
		return nil, err
	}

	return &pendingUpload{
		path:   normalized,
		ctime:  req.CTime,
		mtime:  req.MTime,
		pieces: req.Pieces,
		hash:   decodedHash,
	}, nil
}

func (s *Server) finishUpload(upload pendingUpload) error {
	body, err := vaultcrypto.DecodeFileBody(s.rawKey, s.vault.EncryptionVersion, upload.buffer.Bytes())
	if err != nil {
		return err
	}
	if upload.hash != "" && vaultcrypto.PlaintextHash(body) != upload.hash {
		return errors.New("hash mismatch")
	}

	return s.putFile(upload.path, body, upload.ctime, upload.mtime)
}

func (s *Server) writeSnapshot(conn *websocket.Conn) error {
	s.mu.Lock()
	entries := make([]entry, 0, len(s.entriesByID))
	for _, current := range s.entriesByID {
		entries = append(entries, *current)
	}
	version := s.version
	s.mu.Unlock()

	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Path == entries[j].Path {
			return entries[i].UID < entries[j].UID
		}
		return entries[i].Path < entries[j].Path
	})

	for _, current := range entries {
		payload, err := s.encodePush(current)
		if err != nil {
			return err
		}
		if err := writeWSJSON(conn, payload); err != nil {
			return err
		}
	}

	return writeWSJSON(conn, map[string]any{
		"op":      "ready",
		"version": version,
	})
}

func (s *Server) encodePush(current entry) (pushEnvelope, error) {
	encodedPath, err := vaultcrypto.EncodeMetadata(s.rawKey, s.vault.Salt, s.vault.EncryptionVersion, current.Path)
	if err != nil {
		return pushEnvelope{}, err
	}

	payload := pushEnvelope{
		Op:      "push",
		UID:     current.UID,
		Path:    encodedPath,
		CTime:   current.CTime,
		MTime:   current.MTime,
		Device:  current.Device,
		Folder:  current.Folder,
		Deleted: false,
	}
	if current.Folder {
		return payload, nil
	}

	hash, err := vaultcrypto.EncodeMetadata(s.rawKey, s.vault.Salt, s.vault.EncryptionVersion, vaultcrypto.PlaintextHash(current.Body))
	if err != nil {
		return pushEnvelope{}, err
	}

	payload.Hash = hash
	payload.Size = int64(len(current.Body))

	return payload, nil
}

func (s *Server) putSeedFile(filePath string, body []byte) error {
	now := time.Now().UTC().UnixMilli()
	return s.putFile(filePath, body, now, now)
}

func (s *Server) putFile(filePath string, body []byte, ctime int64, mtime int64) error {
	normalized := normalizePath(filePath)
	if normalized == "" {
		return errors.New("invalid path")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.ensureParentFoldersLocked(normalized); err != nil {
		return err
	}

	if ctime <= 0 {
		ctime = time.Now().UTC().UnixMilli()
	}
	if mtime <= 0 {
		mtime = ctime
	}

	if existing, ok := s.entriesBy[normalized]; ok {
		if existing.Folder {
			return fmt.Errorf("path %q is a folder", normalized)
		}
		existing.Body = append(existing.Body[:0], body...)
		existing.CTime = ctime
		existing.MTime = mtime
		existing.Device = defaultDevice
		s.version++
		return nil
	}

	next := &entry{
		UID:    s.nextUID,
		Path:   normalized,
		Folder: false,
		Body:   append([]byte(nil), body...),
		CTime:  ctime,
		MTime:  mtime,
		Device: defaultDevice,
	}
	s.nextUID++
	s.entriesBy[next.Path] = next
	s.entriesByID[next.UID] = next
	s.version++

	return nil
}

func (s *Server) ensureFolder(folderPath string) error {
	normalized := normalizePath(folderPath)
	if normalized == "" {
		return errors.New("invalid path")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	return s.ensureFolderLocked(normalized)
}

func (s *Server) ensureParentFoldersLocked(filePath string) error {
	dir := path.Dir(filePath)
	for dir != "." && dir != "/" {
		if err := s.ensureFolderLocked(dir); err != nil {
			return err
		}
		dir = path.Dir(dir)
	}

	return nil
}

func (s *Server) ensureFolderLocked(folderPath string) error {
	if existing, ok := s.entriesBy[folderPath]; ok {
		if !existing.Folder {
			return fmt.Errorf("path %q is a file", folderPath)
		}
		return nil
	}

	next := &entry{
		UID:    s.nextUID,
		Path:   folderPath,
		Folder: true,
		CTime:  time.Now().UTC().UnixMilli(),
		MTime:  time.Now().UTC().UnixMilli(),
		Device: defaultDevice,
	}
	s.nextUID++
	s.entriesBy[next.Path] = next
	s.entriesByID[next.UID] = next
	s.version++

	return nil
}

func (s *Server) deletePath(filePath string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	current, ok := s.entriesBy[filePath]
	if !ok {
		return
	}

	delete(s.entriesBy, filePath)
	delete(s.entriesByID, current.UID)
	s.version++
}

func normalizePath(filePath string) string {
	trimmed := strings.TrimSpace(filePath)
	if trimmed == "" {
		return ""
	}

	cleaned := path.Clean(strings.ReplaceAll(trimmed, "\\", "/"))
	if cleaned == "." || cleaned == "/" || strings.HasPrefix(cleaned, "../") || cleaned == ".." {
		return ""
	}

	return cleaned
}

func pieceCount(size int) int {
	if size <= 0 {
		return 0
	}

	const pieceSize = 2 * 1024 * 1024
	return (size + pieceSize - 1) / pieceSize
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	body, err := json.Marshal(payload)
	if err != nil {
		http.Error(w, "encode response", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{
		"message": message,
	})
}

func decodeJSON(w http.ResponseWriter, r *http.Request, target any) bool {
	defer r.Body.Close()

	if err := json.NewDecoder(r.Body).Decode(target); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return false
	}

	return true
}

func writeWSJSON(conn *websocket.Conn, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	return conn.WriteMessage(websocket.TextMessage, body)
}

func withDefault(value string, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}

	return value
}
