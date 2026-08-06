//go:build windows

package dialog

import (
	"syscall"
	"unsafe"
)

// Info shows a modal information dialog (works from GUI / tray apps).
func Info(title, text string) {
	t, _ := syscall.UTF16PtrFromString(title)
	m, _ := syscall.UTF16PtrFromString(text)
	user32 := syscall.NewLazyDLL("user32.dll")
	proc := user32.NewProc("MessageBoxW")
	const mbOK = 0x00000000
	const mbIconInformation = 0x00000040
	const mbSetForeground = 0x00010000
	const mbTopmost = 0x00040000
	_, _, _ = proc.Call(
		0,
		uintptr(unsafe.Pointer(m)),
		uintptr(unsafe.Pointer(t)),
		uintptr(mbOK|mbIconInformation|mbSetForeground|mbTopmost),
	)
}
