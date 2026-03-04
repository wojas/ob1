package remotelist

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
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
	MTime  int64
	Device string
	Folder bool
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
	MTime   int64  `json:"mtime"`
	Device  string `json:"device"`
	Folder  bool   `json:"folder"`
	Deleted bool   `json:"deleted"`
}

func Snapshot(ctx context.Context, logger *slog.Logger, token string, state vaultstore.VaultState) ([]Entry, error) {
	if strings.TrimSpace(token) == "" {
		return nil, errors.New("missing auth token")
	}

	rawKey, err := vaultcrypto.DecodeKey(state.EncryptionKey)
	if err != nil {
		return nil, err
	}

	wsURL, err := websocketURL(state.Host)
	if err != nil {
		return nil, err
	}

	logger.Debug("websocket connect", "url", wsURL)
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, wsURL, nil)
	if err != nil {
		return nil, fmt.Errorf("connect websocket: %w", err)
	}
	defer conn.Close()

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
		return nil, err
	}

	if err := awaitInitOK(logger, conn); err != nil {
		return nil, err
	}

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

			entriesByUID[push.UID] = Entry{
				Path:   path,
				UID:    push.UID,
				Size:   push.Size,
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
