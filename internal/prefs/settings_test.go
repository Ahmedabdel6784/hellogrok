package prefs

import (
	"os"
	"path/filepath"
	"testing"
)

func TestProxyEnabledDefaultsTrueAndPersistsExplicitChoice(t *testing.T) {
	path := Path(t.TempDir())
	if enabled, err := ProxyEnabled(path); err != nil || !enabled {
		t.Fatalf("default enabled=%v err=%v", enabled, err)
	}
	if err := SetProxyEnabled(path, false); err != nil {
		t.Fatal(err)
	}
	if enabled, err := ProxyEnabled(path); err != nil || enabled {
		t.Fatalf("saved false enabled=%v err=%v", enabled, err)
	}
	if err := SetProxyEnabled(path, true); err != nil {
		t.Fatal(err)
	}
	if enabled, err := ProxyEnabled(path); err != nil || !enabled {
		t.Fatalf("saved true enabled=%v err=%v", enabled, err)
	}
}

func TestProxyEnabledRejectsCorruptSettings(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, settingsFileName)
	if err := os.WriteFile(path, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ProxyEnabled(path); err == nil {
		t.Fatal("corrupt settings must not be ignored")
	}
}
