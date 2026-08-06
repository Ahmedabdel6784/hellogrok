//go:build windows

package console

import (
	"os"
	"syscall"
	"unsafe"
)

// Show allocates a console window and binds stdio (for log viewer).
func Show(title string) error {
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	alloc := kernel32.NewProc("AllocConsole")
	setTitle := kernel32.NewProc("SetConsoleTitleW")

	_, _, _ = alloc.Call()
	if title != "" {
		t, err := syscall.UTF16PtrFromString(title)
		if err == nil {
			_, _, _ = setTitle.Call(uintptr(unsafe.Pointer(t)))
		}
	}

	// Bind Go stdio to the new console devices
	out, err := os.OpenFile("CONOUT$", os.O_RDWR, 0)
	if err == nil {
		os.Stdout = out
		os.Stderr = out
	}
	in, err := os.OpenFile("CONIN$", os.O_RDWR, 0)
	if err == nil {
		os.Stdin = in
	}
	return nil
}
