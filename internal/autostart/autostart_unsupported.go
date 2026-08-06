//go:build !windows && !linux && !darwin

package autostart

import (
	"fmt"
	"runtime"
)

func Enabled() bool { return false }

func Set(bool) error {
	return fmt.Errorf("autostart is unsupported on %s", runtime.GOOS)
}

func SetUI(enabled bool) error { return Set(enabled) }
