//go:build !windows

package console

// Show is a no-op; terminal already available on Unix when launched from shell.
func Show(title string) error { return nil }
