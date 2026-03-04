package vaultstore

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const (
	configDirName             = ".ob1"
	vaultFileName             = "vault.json"
	configDirPerm os.FileMode = 0o700
	vaultFilePerm os.FileMode = 0o600
)

type Store struct {
	path string
}

type VaultState struct {
	VaultID           string    `json:"vault_id"`
	VaultName         string    `json:"vault_name"`
	Host              string    `json:"host"`
	Region            string    `json:"region,omitempty"`
	EncryptionVersion int       `json:"encryption_version"`
	EncryptionKey     string    `json:"encryption_key"`
	Salt              string    `json:"salt"`
	KeyHash           string    `json:"keyhash"`
	ConflictStrategy  string    `json:"conflict_strategy"`
	DeviceName        string    `json:"device_name"`
	UserEmail         string    `json:"user_email"`
	SyncVersion       int64     `json:"sync_version"`
	NeedsInitialSync  bool      `json:"needs_initial_sync"`
	APIBaseURL        string    `json:"api_base_url,omitempty"`
	ConfiguredAt      time.Time `json:"configured_at"`
}

func NewInDir(root string) *Store {
	return &Store{
		path: filepath.Join(root, configDirName, vaultFileName),
	}
}

func (s *Store) Path() string {
	return s.path
}

func (s *Store) Load() (VaultState, error) {
	body, err := os.ReadFile(s.path)
	if err != nil {
		return VaultState{}, err
	}

	var state VaultState
	if err := json.Unmarshal(body, &state); err != nil {
		return VaultState{}, fmt.Errorf("decode %s: %w", s.path, err)
	}

	if state.VaultID == "" {
		return VaultState{}, errors.New("vault config does not contain a vault id")
	}
	if state.Host == "" {
		return VaultState{}, errors.New("vault config does not contain a host")
	}
	if state.EncryptionKey == "" {
		return VaultState{}, errors.New("vault config does not contain an encryption key")
	}

	return state, nil
}

func (s *Store) Save(state VaultState) error {
	if state.VaultID == "" {
		return errors.New("cannot save empty vault id")
	}
	if state.EncryptionKey == "" {
		return errors.New("cannot save empty encryption key")
	}

	if err := os.MkdirAll(filepath.Dir(s.path), configDirPerm); err != nil {
		return fmt.Errorf("create vault config directory: %w", err)
	}

	body, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode vault state: %w", err)
	}
	body = append(body, '\n')

	tempFile, err := os.CreateTemp(filepath.Dir(s.path), vaultFileName+".*.tmp")
	if err != nil {
		return fmt.Errorf("create temp vault file: %w", err)
	}

	tempPath := tempFile.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tempPath)
		}
	}()

	if err := tempFile.Chmod(vaultFilePerm); err != nil {
		_ = tempFile.Close()
		return fmt.Errorf("chmod temp vault file: %w", err)
	}

	if _, err := tempFile.Write(body); err != nil {
		_ = tempFile.Close()
		return fmt.Errorf("write temp vault file: %w", err)
	}

	if err := tempFile.Sync(); err != nil {
		_ = tempFile.Close()
		return fmt.Errorf("sync temp vault file: %w", err)
	}

	if err := tempFile.Close(); err != nil {
		return fmt.Errorf("close temp vault file: %w", err)
	}

	if err := os.Rename(tempPath, s.path); err != nil {
		return fmt.Errorf("replace vault config: %w", err)
	}

	cleanup = false

	return nil
}
