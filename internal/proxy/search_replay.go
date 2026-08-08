package proxy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	searchReplayTTL           = 30 * time.Minute
	maxSearchReplaysPerRoute  = 128
	maxSearchReplayQueryBytes = 64 << 10
	maxMessagesReplayBytes    = 256 << 10
)

type searchReplayEntry struct {
	queries            []any
	messagesToolUse    map[string]any
	messagesToolResult map[string]any
	updated            time.Time
}

type searchReplayKey struct {
	callID       string
	conversation string
	item         string
}

// searchReplayCache retains provider-only fields that Grok Build's standard
// Responses types cannot round-trip. It is memory-only, bounded, and isolated
// by channel route so identical upstream call IDs cannot cross credentials.
type searchReplayCache struct {
	mu       sync.Mutex
	channels map[string]map[searchReplayKey]searchReplayEntry
}

func newSearchReplayCache() *searchReplayCache {
	return &searchReplayCache{channels: map[string]map[searchReplayKey]searchReplayEntry{}}
}

func (c *searchReplayCache) captureJSON(channel, conversation string, data []byte) {
	if c == nil || channel == "" || len(data) == 0 {
		return
	}
	var value any
	if json.Unmarshal(data, &value) != nil {
		return
	}
	c.captureValue(channel, conversation, value)
}

func (c *searchReplayCache) captureValue(channel, conversation string, value any) {
	if c == nil || channel == "" {
		return
	}
	now := time.Now()
	entries := map[searchReplayKey][]any{}
	var walk func(any)
	walk = func(current any) {
		switch typed := current.(type) {
		case map[string]any:
			if stringValue(typed["type"]) == "web_search_call" {
				id := stringValue(typed["id"])
				action, _ := typed["action"].(map[string]any)
				queries, exists := action["queries"].([]any)
				if id != "" && exists {
					if clone, ok := boundedJSONSlice(queries, maxSearchReplayQueryBytes); ok {
						key := searchReplayKey{
							callID:       id,
							conversation: conversation,
							item:         searchReplayItemFingerprint(typed),
						}
						entries[key] = clone
					}
				}
			}
			for _, child := range typed {
				walk(child)
			}
		case []any:
			for _, child := range typed {
				walk(child)
			}
		}
	}
	walk(value)
	if len(entries) == 0 {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	channelEntries := c.channels[channel]
	if channelEntries == nil {
		channelEntries = map[searchReplayKey]searchReplayEntry{}
		c.channels[channel] = channelEntries
	}
	pruneSearchReplayEntries(channelEntries, now)
	for key, queries := range entries {
		entry := channelEntries[key]
		entry.queries = queries
		entry.updated = now
		channelEntries[key] = entry
	}
	pruneSearchReplayCount(channelEntries)
}

// captureMessages retains the exact server-side search blocks returned by a
// Messages provider. Grok Build's Responses conversation model keeps only a
// web_search_call, so the pair is restored when that item is sent back to the
// original Messages endpoint on a later turn.
func (c *searchReplayCache) captureMessages(channel, conversation string, data []byte) {
	if c == nil || channel == "" || len(data) == 0 {
		return
	}
	var root map[string]any
	if json.Unmarshal(data, &root) != nil {
		return
	}
	toolUses := map[string]map[string]any{}
	toolResults := map[string]map[string]any{}
	for _, raw := range anySlice(root["content"]) {
		block, _ := raw.(map[string]any)
		switch stringValue(block["type"]) {
		case "server_tool_use":
			if stringValue(block["name"]) == "web_search" {
				toolUses[stringValue(block["id"])] = block
			}
		case "web_search_tool_result":
			toolResults[stringValue(block["tool_use_id"])] = block
		}
	}
	if len(toolUses) == 0 || len(toolResults) == 0 {
		return
	}

	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	channelEntries := c.channels[channel]
	if channelEntries == nil {
		channelEntries = map[searchReplayKey]searchReplayEntry{}
		c.channels[channel] = channelEntries
	}
	pruneSearchReplayEntries(channelEntries, now)
	for id, toolUse := range toolUses {
		toolResult := toolResults[id]
		if id == "" || toolResult == nil {
			continue
		}
		useClone, resultClone, ok := boundedMessagesPair(toolUse, toolResult, maxMessagesReplayBytes)
		if !ok {
			continue
		}
		key := searchReplayKey{
			callID:       id,
			conversation: conversation,
			item:         messagesReplayItemFingerprint(toolUse, toolResult),
		}
		entry := channelEntries[key]
		entry.messagesToolUse = useClone
		entry.messagesToolResult = resultClone
		entry.updated = now
		channelEntries[key] = entry
	}
	pruneSearchReplayCount(channelEntries)
}

func (c *searchReplayCache) messagesBlocks(channel, conversation string, item map[string]any) []map[string]any {
	fallback := messagesReplayFallback(item)
	if c == nil || channel == "" || item == nil {
		return fallback
	}
	id := stringValue(item["id"])
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	channelEntries := c.channels[channel]
	pruneSearchReplayEntries(channelEntries, now)
	entry, ok := findSearchReplayEntry(channelEntries, searchReplayKey{
		callID:       id,
		conversation: conversation,
		item:         searchReplayItemFingerprint(item),
	})
	if !ok {
		return fallback
	}
	toolUse, toolResult, valid := boundedMessagesPair(entry.messagesToolUse, entry.messagesToolResult, maxMessagesReplayBytes)
	if !valid {
		return fallback
	}
	return []map[string]any{toolUse, toolResult}
}

func messagesReplayFallback(item map[string]any) []map[string]any {
	if item == nil {
		return nil
	}
	id := stringValue(item["id"])
	if id == "" {
		return nil
	}
	action, _ := item["action"].(map[string]any)
	query := stringValue(action["query"])
	if query == "" {
		query = firstSearchQuery(action["queries"])
	}
	resultContent := make([]any, 0)
	for _, raw := range anySlice(action["sources"]) {
		source, _ := raw.(map[string]any)
		url := stringValue(source["url"])
		if url == "" {
			continue
		}
		result := map[string]any{"type": "web_search_result", "url": url}
		if title := stringValue(source["title"]); title != "" {
			result["title"] = title
		}
		resultContent = append(resultContent, result)
	}
	return []map[string]any{
		{"type": "server_tool_use", "id": id, "name": "web_search", "input": map[string]any{"query": query}},
		{"type": "web_search_tool_result", "tool_use_id": id, "content": resultContent},
	}
}

func (c *searchReplayCache) restore(channel string, root map[string]any) bool {
	if c == nil || channel == "" || root == nil {
		return false
	}
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	channelEntries := c.channels[channel]
	pruneSearchReplayEntries(channelEntries, now)

	changed := false
	input := anySlice(root["input"])
	for index, raw := range input {
		item, _ := raw.(map[string]any)
		if stringValue(item["type"]) != "web_search_call" {
			continue
		}
		action, _ := item["action"].(map[string]any)
		if action == nil || action["queries"] != nil {
			continue
		}
		entry, ok := findSearchReplayEntry(channelEntries, searchReplayKey{
			callID:       stringValue(item["id"]),
			conversation: replayConversationFingerprint(input[:index]),
			item:         searchReplayItemFingerprint(item),
		})
		if !ok {
			continue
		}
		queries, ok := boundedJSONSlice(entry.queries, maxSearchReplayQueryBytes)
		if !ok {
			continue
		}
		action["queries"] = queries
		changed = true
	}
	return changed
}

func findSearchReplayEntry(entries map[searchReplayKey]searchReplayEntry, exact searchReplayKey) (searchReplayEntry, bool) {
	if entry, ok := entries[exact]; ok {
		return entry, true
	}
	var candidate searchReplayEntry
	matches := 0
	for key, entry := range entries {
		if key.callID != exact.callID || key.item != exact.item {
			continue
		}
		candidate = entry
		matches++
		if matches > 1 {
			return searchReplayEntry{}, false
		}
	}
	return candidate, matches == 1
}

func replayConversationFingerprint(value any) string {
	data, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func searchReplayItemFingerprint(item map[string]any) string {
	action, _ := item["action"].(map[string]any)
	query := firstString(action, "query")
	if query == "" {
		query = firstSearchQuery(action["queries"])
	}
	sources := normalizedReplaySources(action["sources"])
	data, _ := json.Marshal(map[string]any{
		"query":   normalizeReplayText(query),
		"sources": sources,
	})
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func messagesReplayItemFingerprint(toolUse, toolResult map[string]any) string {
	input, _ := toolUse["input"].(map[string]any)
	item := map[string]any{
		"action": map[string]any{
			"query":   firstString(input, "query", "q"),
			"sources": messagesReplaySources(toolResult["content"]),
		},
	}
	return searchReplayItemFingerprint(item)
}

func messagesReplaySources(value any) []any {
	var sources []any
	for _, raw := range anySlice(value) {
		entry, _ := raw.(map[string]any)
		if stringValue(entry["type"]) != "web_search_result" || stringValue(entry["url"]) == "" {
			continue
		}
		source := map[string]any{"url": stringValue(entry["url"])}
		if title := stringValue(entry["title"]); title != "" {
			source["title"] = title
		}
		sources = append(sources, source)
	}
	return sources
}

func normalizedReplaySources(value any) []map[string]string {
	sources := make([]map[string]string, 0)
	for _, raw := range anySlice(value) {
		source, _ := raw.(map[string]any)
		url := strings.TrimSpace(stringValue(source["url"]))
		title := normalizeReplayText(stringValue(source["title"]))
		if url == "" && title == "" {
			continue
		}
		identity := map[string]string{}
		if url != "" {
			identity["url"] = url
		} else {
			identity["title"] = title
		}
		sources = append(sources, identity)
	}
	sort.Slice(sources, func(i, j int) bool {
		if sources[i]["url"] == sources[j]["url"] {
			return sources[i]["title"] < sources[j]["title"]
		}
		return sources[i]["url"] < sources[j]["url"]
	})
	return sources
}

func normalizeReplayText(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

func pruneSearchReplayEntries(entries map[searchReplayKey]searchReplayEntry, now time.Time) {
	for key, entry := range entries {
		if now.Sub(entry.updated) > searchReplayTTL {
			delete(entries, key)
		}
	}
}

func pruneSearchReplayCount(entries map[searchReplayKey]searchReplayEntry) {
	for len(entries) > maxSearchReplaysPerRoute {
		var oldestKey searchReplayKey
		var oldest time.Time
		first := true
		for key, entry := range entries {
			if first || entry.updated.Before(oldest) {
				oldestKey, oldest = key, entry.updated
				first = false
			}
		}
		delete(entries, oldestKey)
	}
}

func boundedJSONSlice(values []any, maxBytes int) ([]any, bool) {
	data, err := json.Marshal(values)
	if err != nil || len(data) > maxBytes {
		return nil, false
	}
	var clone []any
	if json.Unmarshal(data, &clone) != nil {
		return nil, false
	}
	return clone, true
}

func boundedMessagesPair(toolUse, toolResult map[string]any, maxBytes int) (map[string]any, map[string]any, bool) {
	if toolUse == nil || toolResult == nil {
		return nil, nil, false
	}
	data, err := json.Marshal([]any{toolUse, toolResult})
	if err != nil || len(data) > maxBytes {
		return nil, nil, false
	}
	var clone []map[string]any
	if json.Unmarshal(data, &clone) != nil || len(clone) != 2 || clone[0] == nil || clone[1] == nil {
		return nil, nil, false
	}
	return clone[0], clone[1], true
}
