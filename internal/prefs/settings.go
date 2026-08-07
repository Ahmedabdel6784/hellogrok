package prefs

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

const settingsFileName = "settings.json"

type settings struct {
	ProxyEnabled bool `json:"proxy_enabled"`
}

func Path(dataDir string) string {
	return filepath.Join(dataDir, settingsFileName)
}

func ProxyEnabled(path string) (bool, error) {
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	var current settings
	if err := json.Unmarshal(raw, &current); err != nil {
		return false, fmt.Errorf("decode settings: %w", err)
	}
	return current.ProxyEnabled, nil
}

func SetProxyEnabled(path string, enabled bool) error {
	raw, err := json.MarshalIndent(settings{ProxyEnabled: enabled}, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".hellogrok-settings-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	committed := false
	defer func() {
		_ = tmp.Close()
		if !committed {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	committed = true
	return nil
}
