//go:build windows

package groksync

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func TestSupplementalLeaderCandidatesRequiresContendedLock(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "leader.lock")
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	leader := leaderInfo{Classification: "Stale", LockPath: &lockPath}
	if got := supplementalLeaderCandidates([]leaderInfo{leader}); len(got) != 0 {
		t.Fatalf("unlocked stale entry was accepted: %+v", got)
	}

	var overlapped windows.Overlapped
	err = windows.LockFileEx(
		windows.Handle(file.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0,
		1,
		0,
		&overlapped,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.UnlockFileEx(windows.Handle(file.Fd()), 0, 1, 0, &overlapped)

	got := supplementalLeaderCandidates([]leaderInfo{leader})
	if len(got) != 1 || got[0].SocketPath == nil {
		t.Fatalf("contended Windows leader lock was not recovered: %+v", got)
	}
	wantSocket := filepath.Join(dir, "leader.sock")
	if *got[0].SocketPath != wantSocket {
		t.Fatalf("socket path = %q, want %q", *got[0].SocketPath, wantSocket)
	}
}
