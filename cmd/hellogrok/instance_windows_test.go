//go:build windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

func TestAcquireNamedMutexRejectsSecondInstanceAndReleasesOwnership(t *testing.T) {
	name := fmt.Sprintf(`Local\hellogrok.Test.%d.%d`, os.Getpid(), time.Now().UnixNano())
	releaseFirst, alreadyRunning, err := acquireNamedMutex(name)
	if err != nil || alreadyRunning {
		t.Fatalf("first acquire alreadyRunning=%v err=%v", alreadyRunning, err)
	}

	releaseSecond, alreadyRunning, err := acquireNamedMutex(name)
	if err != nil || !alreadyRunning {
		t.Fatalf("second acquire alreadyRunning=%v err=%v", alreadyRunning, err)
	}
	releaseSecond()
	releaseFirst()

	releaseThird, alreadyRunning, err := acquireNamedMutex(name)
	if err != nil || alreadyRunning {
		t.Fatalf("acquire after release alreadyRunning=%v err=%v", alreadyRunning, err)
	}
	releaseThird()
}

func TestAcquireNamedMutexRejectsAnotherProcess(t *testing.T) {
	name := fmt.Sprintf(`Local\hellogrok.Test.Process.%d.%d`, os.Getpid(), time.Now().UnixNano())
	release, alreadyRunning, err := acquireNamedMutex(name)
	if err != nil || alreadyRunning {
		t.Fatalf("parent acquire alreadyRunning=%v err=%v", alreadyRunning, err)
	}
	defer release()

	cmd := exec.Command(os.Args[0], "-test.run=^TestAcquireNamedMutexChild$", "-test.count=1")
	cmd.Env = append(os.Environ(), "HELLOGROK_TEST_PARENT_MUTEX="+name)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("child did not observe the parent mutex: %v\n%s", err, output)
	}
}

func TestAcquireNamedMutexChild(t *testing.T) {
	name := os.Getenv("HELLOGROK_TEST_PARENT_MUTEX")
	if name == "" {
		t.Skip("helper process only")
	}
	release, alreadyRunning, err := acquireNamedMutex(name)
	if err != nil || !alreadyRunning {
		t.Fatalf("child acquire alreadyRunning=%v err=%v", alreadyRunning, err)
	}
	release()
}

func TestApplicationIconResourceIsLoadable(t *testing.T) {
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	user32 := syscall.NewLazyDLL("user32.dll")
	hInstance, _, _ := kernel32.NewProc("GetModuleHandleW").Call(0)
	if hInstance == 0 {
		t.Fatal("GetModuleHandleW returned a null module")
	}
	const (
		imageIcon         = 1
		appIconResourceID = 1
		lrShared          = 0x00008000
	)
	icon, _, callErr := user32.NewProc("LoadImageW").Call(
		hInstance,
		appIconResourceID,
		imageIcon,
		32,
		32,
		lrShared,
	)
	if icon == 0 {
		t.Fatalf("LoadImageW could not load application icon resource: %v", callErr)
	}
}
