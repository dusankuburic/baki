package storage

import (
	"os"
	"path/filepath"
)

func ConfigDir() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	appDir := filepath.Join(dir, "pad-analyzer")
	if err := os.MkdirAll(appDir, 0750); err != nil {
		return "", err
	}
	return appDir, nil
}

func SettingsPath() (string, error) {
	dir, err := ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "settings.json"), nil
}
