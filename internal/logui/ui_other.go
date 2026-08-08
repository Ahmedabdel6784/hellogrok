//go:build !windows

package logui

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
)

// StatusFunc returns short title + detail text.
type StatusFunc func() (short, detail string)

// Open falls back to a separate terminal logview on non-Windows.
func Open(
	path string,
	status StatusFunc,
	getRetention func() (int, error),
	setRetention func(int) error,
) error {
	_ = status
	_ = getRetention
	_ = setRetention
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	switch runtime.GOOS {
	case "darwin":
		script := `on run argv
tell application "Terminal" to do script ((quoted form of (item 1 of argv)) & " logview")
end run`
		return exec.Command("osascript", "-e", script, exe).Start()
	default:
		if p, err := exec.LookPath("x-terminal-emulator"); err == nil {
			return exec.Command(p, "-e", exe, "logview").Start()
		}
		if p, err := exec.LookPath("gnome-terminal"); err == nil {
			return exec.Command(p, "--", exe, "logview").Start()
		}
		if p, err := exec.LookPath("xdg-open"); err == nil {
			return exec.Command(p, path).Start()
		}
		return fmt.Errorf("no supported terminal emulator or file opener found")
	}
}
