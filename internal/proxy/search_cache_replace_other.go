//go:build !windows

package proxy

import "os"

func replaceSearchCapabilityFile(source, target string) error {
	return os.Rename(source, target)
}
