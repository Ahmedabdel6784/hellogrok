//go:build !windows

package console

// Hide is a no-op on non-Windows.
func Hide() {}
