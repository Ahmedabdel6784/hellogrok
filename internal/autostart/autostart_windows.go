//go:build windows

package autostart

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows/registry"
)

const valueName = "hellogrok"

func Enabled() bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Run`, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()
	command, _, err := k.GetStringValue(valueName)
	return err == nil && executableAvailable(commandExecutable(command))
}

func Set(enabled bool) error {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Run`, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	if !enabled {
		if err := k.DeleteValue(valueName); err != nil && err != registry.ErrNotExist {
			return err
		}
		return nil
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	exe, _ = filepath.Abs(exe)
	return k.SetStringValue(valueName, fmt.Sprintf(`"%s"`, exe))
}

func SetUI(enabled bool) error { return Set(enabled) }
