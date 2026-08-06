//go:build linux

package autostart

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func Enabled() bool {
	path, err := linuxUnitPath()
	if err != nil {
		return false
	}
	raw, err := os.ReadFile(path)
	if err != nil || !executableAvailable(systemdCommandExecutable(string(raw))) {
		return false
	}
	return runSystemctl("is-enabled", "--quiet", systemdUnitName) == nil
}

func Set(enabled bool) error {
	return set(enabled, false)
}

func SetUI(enabled bool) error {
	return set(enabled, true)
}

func set(enabled, openUI bool) error {
	path, err := linuxUnitPath()
	if err != nil {
		return err
	}
	if !enabled {
		return disableLinux(path)
	}

	if _, err := exec.LookPath("systemctl"); err != nil {
		return fmt.Errorf("systemd user services are unavailable: %w", err)
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	exe, err = filepath.Abs(exe)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	backup, err := writeManagedFile(path, []byte(systemdUnit(exe, openUI)), 0o644)
	if err != nil {
		return err
	}
	if err := runSystemctl("daemon-reload"); err != nil {
		return restoreManagedFileAfterError(path, backup, err)
	}
	if err := runSystemctl("enable", systemdUnitName); err != nil {
		result := restoreManagedFileAfterError(path, backup, err)
		if reloadErr := runSystemctl("daemon-reload"); reloadErr != nil {
			return fmt.Errorf("%w; reload restored systemd unit: %v", result, reloadErr)
		}
		return result
	}
	return nil
}

func disableLinux(path string) error {
	_, statErr := os.Stat(path)
	if _, err := exec.LookPath("systemctl"); err == nil {
		if disableErr := runSystemctl("disable", systemdUnitName); disableErr != nil && statErr == nil {
			return disableErr
		}
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	if _, err := exec.LookPath("systemctl"); err == nil {
		return runSystemctl("daemon-reload")
	}
	return nil
}

func runSystemctl(args ...string) error {
	full := append([]string{"--user"}, args...)
	out, err := exec.Command("systemctl", full...).CombinedOutput()
	if err == nil {
		return nil
	}
	detail := strings.TrimSpace(string(out))
	if detail == "" {
		detail = err.Error()
	}
	return fmt.Errorf("systemctl %s: %s", strings.Join(full, " "), detail)
}

func linuxUnitPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "systemd", "user", systemdUnitName), nil
}
