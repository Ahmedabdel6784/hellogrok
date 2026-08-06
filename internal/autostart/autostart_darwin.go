//go:build darwin

package autostart

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
)

func Enabled() bool {
	path, err := launchAgentPath()
	if err != nil {
		return false
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	arguments := launchAgentArguments(raw)
	if len(arguments) == 0 || len(arguments) > 2 || !executableAvailable(arguments[0]) {
		return false
	}
	if len(arguments) == 2 && arguments[1] != "start" {
		return false
	}
	domain := "gui/" + strconv.Itoa(os.Getuid())
	out, err := exec.Command("launchctl", "print-disabled", domain).CombinedOutput()
	if err != nil {
		return false
	}
	disabled := regexp.MustCompile(`"` + regexp.QuoteMeta(launchAgentLabel) + `"\s*=>\s*true`).Match(out)
	return !disabled
}

func Set(enabled bool) error {
	return set(enabled, false)
}

func SetUI(enabled bool) error {
	return set(enabled, true)
}

func set(enabled, openUI bool) error {
	path, err := launchAgentPath()
	if err != nil {
		return err
	}
	domain := "gui/" + strconv.Itoa(os.Getuid())
	service := domain + "/" + launchAgentLabel
	if !enabled {
		if out, err := exec.Command("launchctl", "disable", service).CombinedOutput(); err != nil {
			return fmt.Errorf("launchctl disable: %s", string(out))
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
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
	backup, err := writeManagedFile(path, []byte(launchAgent(exe, openUI)), 0o644)
	if err != nil {
		return err
	}
	if out, err := exec.Command("launchctl", "enable", service).CombinedOutput(); err != nil {
		cause := fmt.Errorf("launchctl enable: %s", string(out))
		return restoreManagedFileAfterError(path, backup, cause)
	}
	return nil
}

func launchAgentPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", launchAgentLabel+".plist"), nil
}
