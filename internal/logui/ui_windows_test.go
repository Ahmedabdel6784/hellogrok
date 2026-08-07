//go:build windows

package logui

import "testing"

func TestEditStyleWrapsStatusButKeepsLogLinesUnwrapped(t *testing.T) {
	statusStyle := editStyle(true)
	if statusStyle&wsHScroll != 0 || statusStyle&esAutohscroll != 0 {
		t.Fatalf("wrapped status style enables horizontal scrolling: %#x", statusStyle)
	}
	logStyle := editStyle(false)
	if logStyle&wsHScroll == 0 || logStyle&esAutohscroll == 0 {
		t.Fatalf("unwrapped log style omits horizontal scrolling: %#x", logStyle)
	}
}
