package userstore

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const (
	configDirName = ".ob1"
	userFileName  = "user.json"
	configDirPerm = 0o700
	userFilePerm  = 0o600
)

type Store struct {
	path string
}

type UserState struct {
	APIBaseURL string          `json:"api_base_url"`
	Token      string          `json:"token"`
	User       json.RawMessage `json:"user,omitempty"`
	SavedAt    time.Time       `json:"saved_at"`
}

func NewDefault() (*Store, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("resolve home directory: %w", err)
	}

	return &Store{
		path: filepath.Join(homeDir, configDirName, userFileName),
	}, nil
}

func (s *Store) Path() string {
	return s.path
}

func (s *Store) Load() (UserState, error) {
	body, err := os.ReadFile(s.path)
	if err != nil {
		return UserState{}, err
	}

	var state UserState
	if err := json.Unmarshal(body, &state); err != nil {
		return UserState{}, fmt.Errorf("decode %s: %w", s.path, err)
	}

	if state.Token == "" {
		return UserState{}, errors.New("user store does not contain a token")
	}

	return state, nil
}

func (s *Store) Save(state UserState) error {
	if state.Token == "" {
		return errors.New("cannot save empty token")
	}

	if err := os.MkdirAll(filepath.Dir(s.path), configDirPerm); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}

	body, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode user state: %w", err)
	}
	body = append(body, '\n')

	tempFile, err := os.CreateTemp(filepath.Dir(s.path), userFileName+".*.tmp")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}

	tempPath := tempFile.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tempPath)
		}
	}()

	if err := tempFile.Chmod(userFilePerm); err != nil {
		_ = tempFile.Close()
		return fmt.Errorf("chmod temp file: %w", err)
	}

	if _, err := tempFile.Write(body); err != nil {
		_ = tempFile.Close()
		return fmt.Errorf("write temp file: %w", err)
	}

	if err := tempFile.Sync(); err != nil {
		_ = tempFile.Close()
		return fmt.Errorf("sync temp file: %w", err)
	}

	if err := tempFile.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}

	if err := os.Rename(tempPath, s.path); err != nil {
		return fmt.Errorf("replace user store: %w", err)
	}

	cleanup = false

	return nil
}

func (s *Store) Delete() error {
	err := os.Remove(s.path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("delete %s: %w", s.path, err)
	}

	return nil
}
