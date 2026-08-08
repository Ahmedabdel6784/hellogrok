package proxy

import (
	"fmt"
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "hellogrok-proxy-test-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "create proxy test directory:", err)
		os.Exit(1)
	}
	for _, name := range []string{"LOCALAPPDATA", "APPDATA", "HOME", "USERPROFILE"} {
		if err := os.Setenv(name, dir); err != nil {
			fmt.Fprintf(os.Stderr, "set proxy test environment variable %s: %v\n", name, err)
			_ = os.RemoveAll(dir)
			os.Exit(1)
		}
	}

	code := m.Run()
	if err := os.RemoveAll(dir); err != nil {
		fmt.Fprintln(os.Stderr, "remove proxy test directory:", err)
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}
