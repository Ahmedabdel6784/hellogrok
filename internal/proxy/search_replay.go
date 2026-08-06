package proxy

import (
	"encoding/json"
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

// searchReplayCache retains provider-only fields that Grok Build's standard
// Responses types cannot round-trip. It is memory-only, bounded, and isolated
// by channel route so identical upstream call IDs cannot cross credentials.
type searchReplayCache struct {
	mu       sync.Mutex
	channels map[string]map[string]searchReplayEntry
}

func newSearchReplayCache() *searchReplayCache {
	return &searchReplayCache{channels: map[string]map[string]searchReplayEntry{}}
}

func (c *searchReplayCache) captureJSON(channel string, data []byte) {
	if c == nil || channel == "" || len(data) == 0 {
		return
	}
	var value any
	if json.Unmarshal(data, &value) != nil {
		return
	}
	c.captureValue(channel, value)
}

func (c *searchReplayCache) captureValue(channel string, value any) {
	if c == nil || channel == "" {
		return
	}
	now := time.Now()
	entries := map[string][]any{}
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
						entries[id] = clone
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
		channelEntries = map[string]searchReplayEntry{}
		c.channels[channel] = channelEntries
	}
	pruneSearchReplayEntries(channelEntries, now)
	for id, queries := range entries {
		entry := channelEntries[id]
		entry.queries = queries
		entry.updated = now
		channelEntries[id] = entry
	}
	for len(channelEntries) > maxSearchReplaysPerRoute {
		oldestID := ""
		var oldest time.Time
		for id, entry := range channelEntries {
			if oldestID == "" || entry.updated.Before(oldest) {
				oldestID, oldest = id, entry.updated
			}
		}
		delete(channelEntries, oldestID)
	}
}

// captureMessages retains the exact server-side search blocks returned by a
// Messages provider. Grok Build's Responses conversation model keeps only a
// web_search_call, so the pair is restored when that item is sent back to the
// original Messages endpoint on a later turn.
func (c *searchReplayCache) captureMessages(channel string, data []byte) {
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
	if len(toolUses) == 0 && len(toolResults) == 0 {
		return
	}

	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	channelEntries := c.channels[channel]
	if channelEntries == nil {
		channelEntries = map[string]searchReplayEntry{}
		c.channels[channel] = channelEntries
	}
	pruneSearchReplayEntries(channelEntries, now)
	for id, block := range toolUses {
		if id == "" {
			continue
		}
		clone, ok := boundedJSONObject(block, maxMessagesReplayBytes)
		if !ok {
			continue
		}
		entry := channelEntries[id]
		entry.messagesToolUse = clone
		entry.updated = now
		channelEntries[id] = entry
	}
	for id, block := range toolResults {
		if id == "" {
			continue
		}
		clone, ok := boundedJSONObject(block, maxMessagesReplayBytes)
		if !ok {
			continue
		}
		entry := channelEntries[id]
		entry.messagesToolResult = clone
		entry.updated = now
		channelEntries[id] = entry
	}
	pruneSearchReplayCount(channelEntries)
}

func (c *searchReplayCache) messagesBlocks(channel string, item map[string]any) []map[string]any {
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
	entry, ok := channelEntries[id]
	if !ok {
		return fallback
	}
	blocks := make([]map[string]any, 0, 2)
	if clone, valid := boundedJSONObject(entry.messagesToolUse, maxMessagesReplayBytes); valid {
		blocks = append(blocks, clone)
	} else if len(fallback) > 0 {
		blocks = append(blocks, fallback[0])
	}
	if clone, valid := boundedJSONObject(entry.messagesToolResult, maxMessagesReplayBytes); valid {
		blocks = append(blocks, clone)
	} else if len(fallback) > 1 {
		blocks = append(blocks, fallback[1])
	}
	if len(blocks) == 0 {
		return fallback
	}
	return blocks
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
	for _, raw := range anySlice(root["input"]) {
		item, _ := raw.(map[string]any)
		if stringValue(item["type"]) != "web_search_call" {
			continue
		}
		action, _ := item["action"].(map[string]any)
		if action == nil || action["queries"] != nil {
			continue
		}
		entry, ok := channelEntries[stringValue(item["id"])]
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

func pruneSearchReplayEntries(entries map[string]searchReplayEntry, now time.Time) {
	for id, entry := range entries {
		if now.Sub(entry.updated) > searchReplayTTL {
			delete(entries, id)
		}
	}
}

func pruneSearchReplayCount(entries map[string]searchReplayEntry) {
	for len(entries) > maxSearchReplaysPerRoute {
		oldestID := ""
		var oldest time.Time
		for id, entry := range entries {
			if oldestID == "" || entry.updated.Before(oldest) {
				oldestID, oldest = id, entry.updated
			}
		}
		delete(entries, oldestID)
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

func boundedJSONObject(value map[string]any, maxBytes int) (map[string]any, bool) {
	if value == nil {
		return nil, false
	}
	data, err := json.Marshal(value)
	if err != nil || len(data) > maxBytes {
		return nil, false
	}
	var clone map[string]any
	if json.Unmarshal(data, &clone) != nil {
		return nil, false
	}
	return clone, true
}
