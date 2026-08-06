package autostart

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestSystemdUnitQuotesExecutableAndStartsProxy(t *testing.T) {
	executable := `/home/a user/100%/hellogrok`
	unit := systemdUnit(executable, false)
	if !strings.Contains(unit, `ExecStart="/home/a user/100%%/hellogrok" start`) {
		t.Fatalf("unsafe or incomplete ExecStart:\n%s", unit)
	}
	if !strings.Contains(unit, "WantedBy=default.target") {
		t.Fatalf("unit is not enableable:\n%s", unit)
	}
	if parsed := systemdCommandExecutable(unit); parsed != executable {
		t.Fatalf("parsed executable = %q, want %q", parsed, executable)
	}
}

func TestSystemdUIUnitOpensTrayWithoutStartArgument(t *testing.T) {
	unit := systemdUnit(`/home/user/hellogrok-tray`, true)
	if !strings.Contains(unit, `ExecStart="/home/user/hellogrok-tray"`) || strings.Contains(unit, `hellogrok-tray" start`) {
		t.Fatalf("UI ExecStart is incorrect:\n%s", unit)
	}
}

func TestLaunchAgentEscapesPathAndStartsProxy(t *testing.T) {
	executable := `/Users/a&b/<proxy>/hellogrok`
	plist := launchAgent(executable, false)
	if strings.Contains(plist, `/Users/a&b/<proxy>`) {
		t.Fatalf("path was not XML-escaped:\n%s", plist)
	}
	if !strings.Contains(plist, `/Users/a&amp;b/&lt;proxy&gt;/hellogrok`) {
		t.Fatalf("escaped path missing:\n%s", plist)
	}
	if !strings.Contains(plist, "<string>start</string>") {
		t.Fatalf("proxy start argument missing:\n%s", plist)
	}
	arguments := launchAgentArguments([]byte(plist))
	if len(arguments) != 2 || arguments[0] != executable || arguments[1] != "start" {
		t.Fatalf("parsed ProgramArguments = %#v", arguments)
	}
}

func TestLaunchAgentUIOpensTrayWithoutStartArgument(t *testing.T) {
	plist := launchAgent(`/Users/user/hellogrok-tray`, true)
	if strings.Contains(plist, "<string>start</string>") {
		t.Fatalf("UI LaunchAgent must not use the headless start command:\n%s", plist)
	}
	if !strings.Contains(plist, "<string>Interactive</string>") {
		t.Fatalf("UI LaunchAgent must be interactive:\n%s", plist)
	}
	arguments := launchAgentArguments([]byte(plist))
	if len(arguments) != 1 || arguments[0] != `/Users/user/hellogrok-tray` {
		t.Fatalf("parsed UI ProgramArguments = %#v", arguments)
	}
}

func TestCommandExecutableParsesQuotedAndPlainCommands(t *testing.T) {
	if got := commandExecutable(`"C:\Program Files\hellogrok.exe"`); got != `C:\Program Files\hellogrok.exe` {
		t.Fatalf("quoted executable = %q", got)
	}
	if got := commandExecutable(`/opt/hellogrok start`); got != `/opt/hellogrok` {
		t.Fatalf("plain executable = %q", got)
	}
}

func TestManagedFileRollbackRestoresExistingContentAndMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "autostart.conf")
	original := []byte("user-owned configuration\n")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	backup, err := writeManagedFile(path, []byte("hellogrok configuration\n"), 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if err := restoreManagedFileAfterError(path, backup, os.ErrPermission); err == nil {
		t.Fatal("the activation error must be returned")
	}
	current, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(current) != string(original) {
		t.Fatalf("rollback content = %q, want %q", current, original)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("rollback mode = %o, want 600", info.Mode().Perm())
	}
}

func TestManagedFileRollbackRemovesNewFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "autostart.conf")
	backup, err := writeManagedFile(path, []byte("hellogrok configuration\n"), 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if err := restoreManagedFile(path, backup); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("new file remains after rollback: %v", err)
	}
}
