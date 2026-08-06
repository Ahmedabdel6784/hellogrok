//go:build windows

package console

import "syscall"

// Hide detaches from and hides the console window (tray mode).
func Hide() {
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	// FreeConsole: detach from current console so double-click shows no black window
	proc := kernel32.NewProc("FreeConsole")
	_, _, _ = proc.Call()
}
