//go:build windows

package openpath

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Open opens a file/URL with the default application (no console flash).
func Open(path string) error {
	operation, err := windows.UTF16PtrFromString("open")
	if err != nil {
		return err
	}
	target, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	shellExecute := windows.NewLazySystemDLL("shell32.dll").NewProc("ShellExecuteW")
	const swShowNormal = 1
	result, _, callErr := shellExecute.Call(
		0,
		uintptr(unsafe.Pointer(operation)),
		uintptr(unsafe.Pointer(target)),
		0,
		0,
		swShowNormal,
	)
	if result <= 32 {
		return fmt.Errorf("ShellExecuteW failed with code %d: %v", result, callErr)
	}
	return nil
}
