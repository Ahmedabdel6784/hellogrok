package logui

import "testing"

func TestNextMatchCyclesCaseInsensitivelyAndWraps(t *testing.T) {
	text := "first ERROR\nsecond error\n"
	start, end, ok := nextMatch(text, "error", 0)
	if !ok || text[start:end] != "ERROR" {
		t.Fatalf("first match = (%d,%d,%t) %q", start, end, ok, text[start:end])
	}
	start, end, ok = nextMatch(text, "error", end)
	if !ok || text[start:end] != "error" {
		t.Fatalf("second match = (%d,%d,%t) %q", start, end, ok, text[start:end])
	}
	start, end, ok = nextMatch(text, "error", end)
	if !ok || text[start:end] != "ERROR" {
		t.Fatalf("wrapped match = (%d,%d,%t) %q", start, end, ok, text[start:end])
	}
}

func TestNextMatchRejectsEmptyAndMissingQueries(t *testing.T) {
	for _, query := range []string{"", "   ", "missing"} {
		if _, _, ok := nextMatch("log text", query, 0); ok {
			t.Fatalf("query %q unexpectedly matched", query)
		}
	}
}
