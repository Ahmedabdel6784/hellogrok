//go:build !windows

package dialog

import (
	"os/exec"
	"runtime"
)

// Info shows a simple information dialog.
func Info(title, text string) {
	switch runtime.GOOS {
	case "darwin":
		script := `on run argv
display dialog (item 2 of argv) with title (item 1 of argv) buttons {"OK"} default button "OK"
end run`
		_ = exec.Command("osascript", "-e", script, title, text).Start()
	default:
		// try zenity / notify-send fallbacks
		if err := exec.Command("zenity", "--info", "--title", title, "--text", text).Start(); err == nil {
			return
		}
		_ = exec.Command("notify-send", title, text).Start()
	}
}
