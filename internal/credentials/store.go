// Package credentials stores the OpenRouter API key in the user's config
// directory, readable only by the user.
package credentials

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

const fileName = "credentials.json"

// Store is the location of the credential file.
type Store struct {
	path string
}

type fileContent struct {
	OpenRouterAPIKey string `json:"openrouter_api_key"`
}

// DefaultStore resolves $XDG_CONFIG_HOME/inkling/credentials.json, or
// ~/.config/inkling/credentials.json when XDG_CONFIG_HOME is not set.
func DefaultStore() (Store, error) {
	configHome := os.Getenv("XDG_CONFIG_HOME")
	if configHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return Store{}, fmt.Errorf("credentials: find home directory: %w", err)
		}
		configHome = filepath.Join(home, ".config")
	}
	return Store{path: filepath.Join(configHome, "inkling", fileName)}, nil
}

// Path is the credential file path.
func (s Store) Path() string {
	return s.path
}

// Save writes the key. The directory is created with mode 0700 and the file
// gets mode 0600, also when it already existed with a wider mode.
func (s Store) Save(apiKey string) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("credentials: create directory: %w", err)
	}
	content, err := json.Marshal(fileContent{OpenRouterAPIKey: apiKey})
	if err != nil {
		return fmt.Errorf("credentials: encode: %w", err)
	}
	if err := os.WriteFile(s.path, append(content, '\n'), 0o600); err != nil {
		return fmt.Errorf("credentials: write %s: %w", s.path, err)
	}
	if err := os.Chmod(s.path, 0o600); err != nil {
		return fmt.Errorf("credentials: restrict %s: %w", s.path, err)
	}
	return nil
}

// Load reads the key. found is false when no credential file exists.
func (s Store) Load() (apiKey string, found bool, err error) {
	content, err := os.ReadFile(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("credentials: read %s: %w", s.path, err)
	}
	var parsed fileContent
	if err := json.Unmarshal(content, &parsed); err != nil {
		return "", false, fmt.Errorf("credentials: parse %s: %w", s.path, err)
	}
	if parsed.OpenRouterAPIKey == "" {
		return "", false, fmt.Errorf("credentials: %s has no openrouter_api_key", s.path)
	}
	return parsed.OpenRouterAPIKey, true, nil
}
