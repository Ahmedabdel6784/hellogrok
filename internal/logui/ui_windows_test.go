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

func TestUTF16LengthCountsSurrogatePairsAsTwo(t *testing.T) {
	if got := utf16Length("a😀b"); got != 4 {
		t.Fatalf("surrogate-pair selection offset = %d, want 4", got)
	}
}

func TestStatusPanelHeightPrioritizesLogArea(t *testing.T) {
	if got := statusPanelHeight(500); got != 170 {
		t.Fatalf("default status height = %d, want 170", got)
	}
	if got := statusPanelHeight(240); got != 80 {
		t.Fatalf("compact status height = %d, want 80", got)
	}
}

func TestMigrateLegacyWindowSize(t *testing.T) {
	legacy := migrateLegacyWindowSize(geometryJSON{W: legacyDefaultWinW, H: legacyDefaultWinH})
	if legacy.W != defaultWinW || legacy.H != defaultWinH {
		t.Fatalf("legacy window size = %dx%d, want %dx%d", legacy.W, legacy.H, defaultWinW, defaultWinH)
	}

	custom := geometryJSON{W: 960, H: 720}
	if got := migrateLegacyWindowSize(custom); got != custom {
		t.Fatalf("custom window size changed from %+v to %+v", custom, got)
	}
}
