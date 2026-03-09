package remotelist

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const (
	configDirName                  = ".ob1"
	cacheFileName                  = "cache.json"
	cacheDirPerm       os.FileMode = 0o700
	cacheFilePerm      os.FileMode = 0o600
	cacheSchemaVersion             = 1
)

type CacheStore struct {
	path string
}

type CacheState struct {
	SchemaVersion int                        `json:"schema_version"`
	Version       int64                      `json:"version"`
	Entries       []Entry                    `json:"entries"`
	SavedAt       time.Time                  `json:"saved_at"`
	Extra         map[string]json.RawMessage `json:"-"`
}

func NewCacheStore(root string) *CacheStore {
	return &CacheStore{
		path: filepath.Join(root, configDirName, cacheFileName),
	}
}

func (s *CacheStore) Path() string {
	return s.path
}

func (s *CacheStore) Load() (CacheState, error) {
	body, err := os.ReadFile(s.path)
	if err != nil {
		return CacheState{}, err
	}

	var state CacheState
	if err := json.Unmarshal(body, &state); err != nil {
		return CacheState{}, fmt.Errorf("decode %s: %w", s.path, err)
	}

	return state, nil
}

func (s *CacheStore) Save(state CacheState) error {
	if state.Version < 0 {
		return errors.New("cannot save negative cache version")
	}
	if state.SchemaVersion == 0 {
		state.SchemaVersion = cacheSchemaVersion
	}

	if err := os.MkdirAll(filepath.Dir(s.path), cacheDirPerm); err != nil {
		return fmt.Errorf("create cache directory: %w", err)
	}

	body, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode cache state: %w", err)
	}
	body = append(body, '\n')

	tempFile, err := os.CreateTemp(filepath.Dir(s.path), cacheFileName+".*.tmp")
	if err != nil {
		return fmt.Errorf("create temp cache file: %w", err)
	}

	tempPath := tempFile.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tempPath)
		}
	}()

	if err := tempFile.Chmod(cacheFilePerm); err != nil {
		_ = tempFile.Close()
		return fmt.Errorf("chmod temp cache file: %w", err)
	}

	if _, err := tempFile.Write(body); err != nil {
		_ = tempFile.Close()
		return fmt.Errorf("write temp cache file: %w", err)
	}

	if err := tempFile.Sync(); err != nil {
		_ = tempFile.Close()
		return fmt.Errorf("sync temp cache file: %w", err)
	}

	if err := tempFile.Close(); err != nil {
		return fmt.Errorf("close temp cache file: %w", err)
	}

	if err := os.Rename(tempPath, s.path); err != nil {
		return fmt.Errorf("replace cache file: %w", err)
	}

	cleanup = false

	return nil
}

func (c CacheState) MarshalJSON() ([]byte, error) {
	payload := map[string]any{
		"schema_version": c.SchemaVersion,
		"version":        c.Version,
		"entries":        c.Entries,
		"saved_at":       c.SavedAt,
	}
	if payload["schema_version"] == 0 {
		payload["schema_version"] = cacheSchemaVersion
	}

	for key, value := range c.Extra {
		if _, exists := payload[key]; exists {
			continue
		}
		payload[key] = value
	}

	return json.Marshal(payload)
}

func (c *CacheState) UnmarshalJSON(data []byte) error {
	raw, err := decodeRawObject(data)
	if err != nil {
		return err
	}

	c.SchemaVersion = cacheSchemaVersion
	c.Version = 0
	c.Entries = nil
	c.SavedAt = time.Time{}
	c.Extra = nil

	if err := unmarshalFirst(raw, &c.SchemaVersion, "schema_version", "SchemaVersion"); err != nil {
		return err
	}
	if err := unmarshalFirst(raw, &c.Version, "version", "Version"); err != nil {
		return err
	}
	if err := unmarshalFirst(raw, &c.Entries, "entries", "Entries"); err != nil {
		return err
	}
	if err := unmarshalFirst(raw, &c.SavedAt, "saved_at", "SavedAt"); err != nil {
		return err
	}
	if len(raw) != 0 {
		c.Extra = raw
	}

	return nil
}

func (e Entry) MarshalJSON() ([]byte, error) {
	payload := map[string]any{
		"path":   e.Path,
		"uid":    e.UID,
		"size":   e.Size,
		"pieces": e.Pieces,
		"ctime":  e.CTime,
		"mtime":  e.MTime,
		"hash":   e.Hash,
		"device": e.Device,
		"folder": e.Folder,
	}

	for key, value := range e.Extra {
		if _, exists := payload[key]; exists {
			continue
		}
		payload[key] = value
	}

	return json.Marshal(payload)
}

func (e *Entry) UnmarshalJSON(data []byte) error {
	raw, err := decodeRawObject(data)
	if err != nil {
		return err
	}

	*e = Entry{}

	if err := unmarshalFirst(raw, &e.Path, "path", "Path"); err != nil {
		return err
	}
	if err := unmarshalFirst(raw, &e.UID, "uid", "UID"); err != nil {
		return err
	}
	if err := unmarshalFirst(raw, &e.Size, "size", "Size"); err != nil {
		return err
	}
	if err := unmarshalFirst(raw, &e.Pieces, "pieces", "Pieces"); err != nil {
		return err
	}
	if err := unmarshalFirst(raw, &e.CTime, "ctime", "CTime"); err != nil {
		return err
	}
	if err := unmarshalFirst(raw, &e.MTime, "mtime", "MTime"); err != nil {
		return err
	}
	if err := unmarshalFirst(raw, &e.Hash, "hash", "Hash"); err != nil {
		return err
	}
	if err := unmarshalFirst(raw, &e.Device, "device", "Device"); err != nil {
		return err
	}
	if err := unmarshalFirst(raw, &e.Folder, "folder", "Folder"); err != nil {
		return err
	}
	if len(raw) != 0 {
		e.Extra = raw
	}

	return nil
}

func decodeRawObject(data []byte) (map[string]json.RawMessage, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}

	return raw, nil
}

func unmarshalFirst(raw map[string]json.RawMessage, target any, keys ...string) error {
	for _, key := range keys {
		value, ok := raw[key]
		if !ok {
			continue
		}
		delete(raw, key)
		if err := json.Unmarshal(value, target); err != nil {
			return fmt.Errorf("decode %s: %w", key, err)
		}
		return nil
	}

	return nil
}
