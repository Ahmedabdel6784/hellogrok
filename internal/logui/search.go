package logui

import "strings"

func nextMatch(text, query string, after int) (start, end int, ok bool) {
	query = strings.TrimSpace(query)
	if query == "" || text == "" {
		return 0, 0, false
	}
	if after < 0 || after > len(text) {
		after = 0
	}

	if index := indexFoldASCII(text[after:], query); index >= 0 {
		start = after + index
		return start, start + len(query), true
	}
	if after > 0 {
		if index := indexFoldASCII(text[:after], query); index >= 0 {
			return index, index + len(query), true
		}
	}
	return 0, 0, false
}

func indexFoldASCII(text, query string) int {
	if len(query) > len(text) {
		return -1
	}
	for start := 0; start+len(query) <= len(text); start++ {
		matched := true
		for offset := range len(query) {
			left, right := text[start+offset], query[offset]
			if left >= 'A' && left <= 'Z' {
				left += 'a' - 'A'
			}
			if right >= 'A' && right <= 'Z' {
				right += 'a' - 'A'
			}
			if left != right {
				matched = false
				break
			}
		}
		if matched {
			return start
		}
	}
	return -1
}
