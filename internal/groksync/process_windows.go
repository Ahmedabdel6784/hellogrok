//go:build windows

package groksync

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/windows"
)

func configureCommand(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: windows.CREATE_NO_WINDOW,
	}
}

// Grok Build 1.0.0 discovers Unix socket files when listing leaders. Windows
// uses named pipes, so a live default leader can be reported as Stale with only
// a lockPath. A contended lock distinguishes that case from a leftover file.
func supplementalLeaderCandidates(all []leaderInfo) []leaderInfo {
	var candidates []leaderInfo
	for _, leader := range all {
		if !strings.EqualFold(strings.TrimSpace(leader.Classification), "stale") {
			continue
		}
		if leader.SocketPath != nil && strings.TrimSpace(*leader.SocketPath) != "" {
			continue
		}
		if leader.LockPath == nil {
			continue
		}
		lockPath := strings.TrimSpace(*leader.LockPath)
		if lockPath == "" || !leaderLockHeld(lockPath) {
			continue
		}
		socketPath := strings.TrimSuffix(lockPath, filepath.Ext(lockPath)) + ".sock"
		leader.SocketPath = &socketPath
		leader.Classification = "Reachable"
		candidates = append(candidates, leader)
	}
	return candidates
}

func leaderLockHeld(path string) bool {
	_, err := os.ReadFile(path)
	return errors.Is(err, windows.ERROR_LOCK_VIOLATION) ||
		errors.Is(err, windows.ERROR_SHARING_VIOLATION)
}
