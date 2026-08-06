package logui

import (
	"bytes"
	"fmt"
	"io"
	"os"
)

func readTailFile(path string, maxBytes int64) ([]byte, int64, error) {
	if maxBytes <= 0 {
		return nil, 0, fmt.Errorf("tail size must be positive")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, 0, err
	}
	size := info.Size()
	start := int64(0)
	if size > maxBytes {
		start = size - maxBytes
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return nil, 0, err
	}
	data, err := io.ReadAll(io.LimitReader(f, maxBytes))
	if err != nil {
		return nil, 0, err
	}
	if start > 0 {
		if newline := bytes.IndexByte(data, '\n'); newline >= 0 {
			data = data[newline+1:]
		} else {
			data = nil
		}
	}
	return data, size, nil
}
