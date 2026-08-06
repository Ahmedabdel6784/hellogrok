package autostart

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

const (
	launchAgentLabel = "com.hellogrok.proxy"
	systemdUnitName  = "hellogrok.service"
)

func systemdUnit(exe string, openUI bool) string {
	command := systemdQuote(exe)
	if !openUI {
		command += " start"
	}
	return fmt.Sprintf(`[Unit]
Description=hellogrok for Grok Build
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=%s
Restart=on-failure
RestartSec=2

[Install]
WantedBy=default.target
`, command)
}

func systemdQuote(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, `"`, `\"`)
	// systemd expands percent specifiers even inside quotes.
	value = strings.ReplaceAll(value, `%`, `%%`)
	return `"` + value + `"`
}

func launchAgent(exe string, openUI bool) string {
	arguments := fmt.Sprintf("    <string>%s</string>\n", xmlText(exe))
	processType := "Background"
	if openUI {
		processType = "Interactive"
	} else {
		arguments += "    <string>start</string>\n"
	}
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>%s</string>
  <key>ProgramArguments</key>
  <array>
%s  </array>
  <key>RunAtLoad</key>
  <true/>
  <key>ProcessType</key>
  <string>%s</string>
</dict>
</plist>
`, launchAgentLabel, arguments, processType)
}

func xmlText(value string) string {
	var out bytes.Buffer
	_ = xml.EscapeText(&out, []byte(value))
	return out.String()
}

func commandExecutable(command string) string {
	command = strings.TrimSpace(command)
	if command == "" {
		return ""
	}
	if command[0] == '"' {
		if end := strings.Index(command[1:], `"`); end >= 0 {
			return command[1 : end+1]
		}
		return ""
	}
	fields := strings.Fields(command)
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

func systemdCommandExecutable(unit string) string {
	for _, line := range strings.Split(unit, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "ExecStart=") {
			continue
		}
		command := strings.TrimSpace(strings.TrimPrefix(line, "ExecStart="))
		if command == "" || command[0] != '"' {
			return commandExecutable(command)
		}
		var out strings.Builder
		for index := 1; index < len(command); index++ {
			switch command[index] {
			case '"':
				return strings.ReplaceAll(out.String(), "%%", "%")
			case '\\':
				index++
				if index >= len(command) {
					return ""
				}
				out.WriteByte(command[index])
			default:
				out.WriteByte(command[index])
			}
		}
		return ""
	}
	return ""
}

func launchAgentArguments(raw []byte) []string {
	decoder := xml.NewDecoder(bytes.NewReader(raw))
	wantArray := false
	inArguments := false
	var arguments []string
	for {
		token, err := decoder.Token()
		if err != nil {
			return nil
		}
		switch element := token.(type) {
		case xml.StartElement:
			switch element.Name.Local {
			case "key":
				var key string
				if decoder.DecodeElement(&key, &element) != nil {
					return nil
				}
				wantArray = key == "ProgramArguments"
			case "array":
				if wantArray {
					inArguments = true
					wantArray = false
				}
			case "string":
				if inArguments {
					var value string
					if decoder.DecodeElement(&value, &element) != nil {
						return nil
					}
					arguments = append(arguments, value)
				}
			}
		case xml.EndElement:
			if element.Name.Local == "array" && inArguments {
				return arguments
			}
		}
	}
}

func executableAvailable(path string) bool {
	info, err := os.Stat(strings.TrimSpace(path))
	if err != nil || info.IsDir() {
		return false
	}
	return runtime.GOOS == "windows" || info.Mode().Perm()&0o111 != 0
}

type managedFileBackup struct {
	data   []byte
	mode   os.FileMode
	exists bool
}

func writeManagedFile(path string, data []byte, mode os.FileMode) (managedFileBackup, error) {
	backup, err := captureManagedFile(path)
	if err != nil {
		return managedFileBackup{}, err
	}
	if err := writeFileAtomic(path, data, mode); err != nil {
		return managedFileBackup{}, err
	}
	return backup, nil
}

func captureManagedFile(path string) (managedFileBackup, error) {
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return managedFileBackup{}, nil
	}
	if err != nil {
		return managedFileBackup{}, err
	}
	if info.IsDir() {
		return managedFileBackup{}, fmt.Errorf("autostart path is a directory: %s", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return managedFileBackup{}, err
	}
	return managedFileBackup{data: data, mode: info.Mode().Perm(), exists: true}, nil
}

func restoreManagedFile(path string, backup managedFileBackup) error {
	if !backup.exists {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	return writeFileAtomic(path, backup.data, backup.mode)
}

func restoreManagedFileAfterError(path string, backup managedFileBackup, cause error) error {
	if err := restoreManagedFile(path, backup); err != nil {
		return fmt.Errorf("%w; restore previous autostart file: %v", cause, err)
	}
	return cause
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".hellogrok-autostart-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	committed := false
	defer func() {
		_ = tmp.Close()
		if !committed {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(mode); err != nil {
		return err
	}
	if _, err := io.Copy(tmp, bytes.NewReader(data)); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	committed = true
	return nil
}
