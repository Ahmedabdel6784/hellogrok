package logretention

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPruneCountsDistinctUsageDatesInsteadOfCalendarAge(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hellogrok.log")
	original := strings.Join([]string{
		"legacy prefix without a date",
		"2026/01/01 09:00:00 first",
		"[legacy-logui] belongs to first",
		"2026/02/10 09:00:00 second",
		"2026/05/20 09:00:00 third",
		"2026/08/01 09:00:00 fourth",
		"",
	}, "\n")
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.Local)
	removed, err := Prune(path, 3, now)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 2 {
		t.Fatalf("removed usage days = %d, want 2", removed)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(got)
	if strings.Contains(text, "2026/01/01") || strings.Contains(text, "2026/02/10") ||
		!strings.Contains(text, "2026/05/20") || !strings.Contains(text, "2026/08/01") {
		t.Fatalf("unexpected retained log:\n%s", text)
	}
	if !strings.Contains(text, "legacy prefix without a date") {
		t.Fatalf("undated legacy prefix was removed:\n%s", text)
	}
}

func TestPruneDisabledAndUndatedLogsAreUntouched(t *testing.T) {
	for _, test := range []struct {
		name string
		keep int
		text string
	}{
		{name: "disabled", keep: 0, text: "2026/01/01 09:00:00 old\n"},
		{name: "undated", keep: 3, text: "custom log without a date\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "hellogrok.log")
			if err := os.WriteFile(path, []byte(test.text), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Prune(path, test.keep, time.Now()); err != nil {
				t.Fatal(err)
			}
			got, _ := os.ReadFile(path)
			if string(got) != test.text {
				t.Fatalf("log changed: %q", got)
			}
		})
	}
}
