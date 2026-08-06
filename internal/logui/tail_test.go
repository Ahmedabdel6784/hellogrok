package logui

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestReadTailFileIsBoundedAndSkipsPartialLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	data := append(bytes.Repeat([]byte("x"), 256), []byte("\nlast line\n")...)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	tail, size, err := readTailFile(path, 32)
	if err != nil {
		t.Fatal(err)
	}
	if size != int64(len(data)) {
		t.Fatalf("size = %d, want %d", size, len(data))
	}
	if string(tail) != "last line\n" {
		t.Fatalf("tail = %q", tail)
	}
	if len(tail) > 32 {
		t.Fatalf("tail exceeded limit: %d", len(tail))
	}
}

func TestReadTailFileRejectsNonPositiveLimit(t *testing.T) {
	if _, _, err := readTailFile("ignored", 0); err == nil {
		t.Fatal("non-positive limit was accepted")
	}
}
