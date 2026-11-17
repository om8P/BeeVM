package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// config is what `beevm login` saves, so the key need not be exported in every terminal.
// It lives in the user's config folder ($XDG_CONFIG_HOME/beevm/config.json, usually
// ~/.config/beevm/config.json) with mode 0600, because the key is a secret.
type config struct {
	URL string `json:"url,omitempty"`
	Key string `json:"key,omitempty"`
}

// configPath returns where the config file lives. BEEVM_CONFIG overrides it (used by tests).
func configPath() (string, error) {
	if p := os.Getenv("BEEVM_CONFIG"); p != "" {
		return p, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "beevm", "config.json"), nil
}

// loadConfig reads the config file. A missing file is an empty config, not an error.
func loadConfig() (config, error) {
	var c config
	p, err := configPath()
	if err != nil {
		return c, nil
	}
	data, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(data, &c); err != nil {
		return c, fmt.Errorf("%s is not valid JSON (delete it, or run beevm login again): %w", p, err)
	}
	return c, nil
}

// saveConfig writes the config file with mode 0600 (written to a temporary file first, so a
// crash never leaves half a file).
func saveConfig(c config) (string, error) {
	p, err := configPath()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return "", err
	}
	data, _ := json.MarshalIndent(c, "", "  ")
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return "", err
	}
	return p, os.Rename(tmp, p)
}

// removeConfig deletes the config file; it is fine if there is none.
func removeConfig() (string, error) {
	p, err := configPath()
	if err != nil {
		return "", err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return p, err
	}
	return p, nil
}
