//go:build windows

package main

import (
	"errors"
	"fmt"

	"golang.org/x/sys/windows"
)

const defaultInstanceMutexName = `Local\hellogrok.Tray.SingleInstance.v1`

func acquireDefaultInstance(_ string) (release func(), alreadyRunning bool, err error) {
	return acquireNamedMutex(defaultInstanceMutexName)
}

func acquireNamedMutex(name string) (release func(), alreadyRunning bool, err error) {
	namePtr, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, false, fmt.Errorf("encode instance mutex name: %w", err)
	}
	handle, err := windows.CreateMutex(nil, false, namePtr)
	if errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		if handle != 0 {
			_ = windows.CloseHandle(handle)
		}
		return func() {}, true, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("create instance mutex: %w", err)
	}
	return func() { _ = windows.CloseHandle(handle) }, false, nil
}
