package client

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// FileConfig is the saved login, so that `burrow ssh` works with no flags.
type FileConfig struct {
	Server   string `json:"server"`
	Token    string `json:"token"`
	NoTLS    bool   `json:"no_tls,omitempty"`
	Insecure bool   `json:"insecure,omitempty"`
	TLSName  string `json:"tls_name,omitempty"`
}

// ConfigPath returns the config file location, honouring XDG_CONFIG_HOME.
func ConfigPath() (string, error) {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("locate home directory: %w", err)
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "burrow", "config.json"), nil
}

// LoadFileConfig reads the saved login. A missing file is not an error: it
// just means nobody has run `burrow login` yet.
func LoadFileConfig() (FileConfig, error) {
	path, err := ConfigPath()
	if err != nil {
		return FileConfig{}, err
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return FileConfig{}, nil
	}
	if err != nil {
		return FileConfig{}, fmt.Errorf("read %s: %w", path, err)
	}
	var cfg FileConfig
	if err := json.Unmarshal(b, &cfg); err != nil {
		return FileConfig{}, fmt.Errorf("parse %s: %w", path, err)
	}
	return cfg, nil
}

// SaveFileConfig writes the login with owner-only permissions. It holds a
// token, so the file must never be group- or world-readable.
func SaveFileConfig(cfg FileConfig) (string, error) {
	path, err := ConfigPath()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", fmt.Errorf("create config directory: %w", err)
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, append(b, '\n'), 0o600); err != nil {
		return "", fmt.Errorf("write %s: %w", path, err)
	}
	return path, nil
}

// DeleteFileConfig removes the saved login.
func DeleteFileConfig() (string, error) {
	path, err := ConfigPath()
	if err != nil {
		return "", err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("remove %s: %w", path, err)
	}
	return path, nil
}
