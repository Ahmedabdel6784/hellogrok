package logretention

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const logDateLayout = "2006/01/02"

// Prune keeps the most recent distinct usage dates found in a log. Calendar
// gaps do not count: a file used on seven non-consecutive dates retains all
// seven dates when keepUsageDays is seven.
func Prune(path string, keepUsageDays int, currentUsage time.Time) (int, error) {
	if keepUsageDays <= 0 {
		return 0, nil
	}
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if len(raw) == 0 {
		return 0, nil
	}

	lines := strings.SplitAfter(string(raw), "\n")
	dates := map[string]struct{}{}
	actualDates := 0
	for _, line := range lines {
		if date, ok := lineDate(line); ok {
			if _, exists := dates[date]; !exists {
				actualDates++
			}
			dates[date] = struct{}{}
		}
	}
	if actualDates == 0 {
		return 0, nil
	}
	if !currentUsage.IsZero() {
		dates[currentUsage.Format(logDateLayout)] = struct{}{}
	}
	ordered := make([]string, 0, len(dates))
	for date := range dates {
		ordered = append(ordered, date)
	}
	sort.Strings(ordered)
	if len(ordered) <= keepUsageDays {
		return 0, nil
	}
	cutoff := ordered[len(ordered)-keepUsageDays]

	var kept strings.Builder
	kept.Grow(len(raw))
	activeDate := ""
	for _, line := range lines {
		if date, ok := lineDate(line); ok {
			activeDate = date
		}
		// Preserve an undated prefix from older hellogrok versions. Once a dated
		// record begins, following continuation lines belong to that usage date.
		if activeDate == "" || activeDate >= cutoff {
			kept.WriteString(line)
		}
	}
	if err := replace(path, []byte(kept.String())); err != nil {
		return 0, err
	}
	return len(ordered) - keepUsageDays, nil
}

func lineDate(line string) (string, bool) {
	if len(line) < len(logDateLayout)+1 || line[len(logDateLayout)] != ' ' {
		return "", false
	}
	value := line[:len(logDateLayout)]
	if _, err := time.Parse(logDateLayout, value); err != nil {
		return "", false
	}
	return value, true
}

func replace(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".hellogrok-log-*")
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
	if err := tmp.Chmod(0o600); err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("replace pruned log: %w", err)
	}
	committed = true
	return nil
}
