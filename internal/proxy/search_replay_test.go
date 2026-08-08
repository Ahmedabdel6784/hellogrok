package proxy

import (
	"encoding/json"
	"testing"
)

func TestSearchReplaySeparatesConversationsWithReusedCallID(t *testing.T) {
	cache := newSearchReplayCache()
	prefixA := []any{map[string]any{"type": "message", "role": "user", "content": "conversation-a"}}
	prefixB := []any{map[string]any{"type": "message", "role": "user", "content": "conversation-b"}}
	cache.captureJSON("shared", replayConversationFingerprint(prefixA), replayResponse(t, "reused-id", "query-a", "https://a.test", "private-a"))
	cache.captureJSON("shared", replayConversationFingerprint(prefixB), replayResponse(t, "reused-id", "query-b", "https://b.test", "private-b"))

	rootA := replayRequest(prefixA, "reused-id", "query-a", "https://a.test")
	if !cache.restore("shared", rootA) {
		t.Fatal("conversation A replay was not restored")
	}
	if got := restoredReplayQuery(rootA); got != "private-a" {
		t.Fatalf("conversation A restored %q, want private-a", got)
	}

	rootB := replayRequest(prefixB, "reused-id", "query-b", "https://b.test")
	if !cache.restore("shared", rootB) {
		t.Fatal("conversation B replay was not restored")
	}
	if got := restoredReplayQuery(rootB); got != "private-b" {
		t.Fatalf("conversation B restored %q, want private-b", got)
	}
}

func TestMessagesReplayDoesNotCrossConversation(t *testing.T) {
	cache := newSearchReplayCache()
	prefixA := []any{map[string]any{"type": "message", "role": "user", "content": "conversation-a"}}
	prefixB := []any{map[string]any{"type": "message", "role": "user", "content": "conversation-b"}}
	cache.captureMessages("shared", replayConversationFingerprint(prefixA), messagesReplayResponse(t, "reused-id", "query-a", "https://a.test", "opaque-a"))
	cache.captureMessages("shared", replayConversationFingerprint(prefixB), messagesReplayResponse(t, "reused-id", "query-b", "https://b.test", "opaque-b"))

	blocksA := replayedMessagesBlocks(t, cache, prefixA, "reused-id", "query-a", "https://a.test")
	if got := replayEncryptedContent(blocksA); got != "opaque-a" {
		t.Fatalf("conversation A restored %q, want opaque-a", got)
	}
	blocksB := replayedMessagesBlocks(t, cache, prefixB, "reused-id", "query-b", "https://b.test")
	if got := replayEncryptedContent(blocksB); got != "opaque-b" {
		t.Fatalf("conversation B restored %q, want opaque-b", got)
	}
}

func TestSearchReplayRejectsAmbiguousFallback(t *testing.T) {
	cache := newSearchReplayCache()
	prefixA := []any{map[string]any{"type": "message", "role": "user", "content": "conversation-a"}}
	prefixB := []any{map[string]any{"type": "message", "role": "user", "content": "conversation-b"}}
	cache.captureJSON("shared", replayConversationFingerprint(prefixA), replayResponse(t, "reused-id", "same-query", "https://same.test", "private-a"))
	cache.captureJSON("shared", replayConversationFingerprint(prefixB), replayResponse(t, "reused-id", "same-query", "https://same.test", "private-b"))

	prefixUnknown := []any{map[string]any{"type": "message", "role": "user", "content": "unknown-conversation"}}
	root := replayRequest(prefixUnknown, "reused-id", "same-query", "https://same.test")
	if cache.restore("shared", root) {
		t.Fatal("ambiguous replay was restored")
	}
	item := anySlice(root["input"])[len(prefixUnknown)].(map[string]any)
	action := item["action"].(map[string]any)
	if _, exists := action["queries"]; exists {
		t.Fatalf("ambiguous replay injected queries: %#v", action)
	}
}

func TestMessagesReplayStoresToolUseAndResultAtomically(t *testing.T) {
	cache := newSearchReplayCache()
	prefix := []any{map[string]any{"type": "message", "role": "user", "content": "conversation"}}
	scope := replayConversationFingerprint(prefix)
	cache.captureMessages("shared", scope, []byte(`{
		"content":[{"type":"server_tool_use","id":"reused-id","name":"web_search","input":{"query":"query"}}]
	}`))
	cache.captureMessages("shared", scope, []byte(`{
		"content":[{"type":"web_search_tool_result","tool_use_id":"reused-id","content":[{"type":"web_search_result","url":"https://result.test","encrypted_content":"must-not-appear"}]}]
	}`))

	blocks := replayedMessagesBlocks(t, cache, prefix, "reused-id", "query", "https://result.test")
	if got := replayEncryptedContent(blocks); got != "" {
		t.Fatalf("separately captured blocks were combined: %q", got)
	}
	if len(blocks) != 2 || stringValue(blocks[0]["type"]) != "server_tool_use" || stringValue(blocks[1]["type"]) != "web_search_tool_result" {
		t.Fatalf("fallback blocks are malformed: %#v", blocks)
	}
}

func replayResponse(t *testing.T, id, query, sourceURL, privateQuery string) []byte {
	t.Helper()
	data, err := json.Marshal(map[string]any{
		"type": "response.completed",
		"response": map[string]any{
			"output": []any{replaySearchItem(id, query, sourceURL, []any{privateQuery})},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func replayRequest(prefix []any, id, query, sourceURL string) map[string]any {
	input := append([]any{}, prefix...)
	input = append(input, replaySearchItem(id, query, sourceURL, nil))
	return map[string]any{"input": input}
}

func replaySearchItem(id, query, sourceURL string, queries []any) map[string]any {
	action := map[string]any{
		"type":    "search",
		"query":   query,
		"sources": []any{map[string]any{"type": "url", "url": sourceURL, "title": "Result"}},
	}
	if queries != nil {
		action["queries"] = queries
	}
	return map[string]any{"type": "web_search_call", "id": id, "action": action}
}

func restoredReplayQuery(root map[string]any) string {
	items := anySlice(root["input"])
	item, _ := items[len(items)-1].(map[string]any)
	action, _ := item["action"].(map[string]any)
	queries := anySlice(action["queries"])
	if len(queries) == 0 {
		return ""
	}
	return stringValue(queries[0])
}

func messagesReplayResponse(t *testing.T, id, query, sourceURL, encrypted string) []byte {
	t.Helper()
	data, err := json.Marshal(map[string]any{
		"content": []any{
			map[string]any{"type": "server_tool_use", "id": id, "name": "web_search", "input": map[string]any{"query": query}},
			map[string]any{"type": "web_search_tool_result", "tool_use_id": id, "content": []any{
				map[string]any{"type": "web_search_result", "url": sourceURL, "title": "Result", "encrypted_content": encrypted},
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func replayedMessagesBlocks(t *testing.T, cache *searchReplayCache, prefix []any, id, query, sourceURL string) []map[string]any {
	t.Helper()
	input := append([]any{}, prefix...)
	input = append(input, replaySearchItem(id, query, sourceURL, nil))
	messages, _, err := responsesInputToMessages(input, true, "shared", cache)
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range messages {
		message, _ := raw.(map[string]any)
		if stringValue(message["role"]) != "assistant" {
			continue
		}
		var blocks []map[string]any
		for _, rawBlock := range anySlice(message["content"]) {
			block, _ := rawBlock.(map[string]any)
			if typ := stringValue(block["type"]); typ == "server_tool_use" || typ == "web_search_tool_result" {
				blocks = append(blocks, block)
			}
		}
		if len(blocks) > 0 {
			return blocks
		}
	}
	t.Fatal("Messages replay blocks were not produced")
	return nil
}

func replayEncryptedContent(blocks []map[string]any) string {
	for _, block := range blocks {
		if stringValue(block["type"]) != "web_search_tool_result" {
			continue
		}
		for _, raw := range anySlice(block["content"]) {
			result, _ := raw.(map[string]any)
			if encrypted := stringValue(result["encrypted_content"]); encrypted != "" {
				return encrypted
			}
		}
	}
	return ""
}
