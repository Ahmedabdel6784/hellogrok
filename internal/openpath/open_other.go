//go:build !windows

package openpath

import (
	"os/exec"
	"runtime"
)

// Open opens a file/URL with the default application.
func Open(path string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", path)
	default:
		cmd = exec.Command("xdg-open", path)
	}
	return cmd.Start()
}
