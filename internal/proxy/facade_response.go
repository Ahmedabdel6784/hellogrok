package proxy

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/hellowind777/hellogrok/internal/config"
)

type canonicalResult struct {
	Output           []any
	InputTokens      int
	OutputTokens     int
	CachedTokens     int
	ReasoningTokens  int
	IncompleteReason string
}

func canonicalFromMessages(data []byte) (canonicalResult, error) {
	root, err := decodeJSONMap(data)
	if err != nil {
		return canonicalResult{}, err
	}
	var result canonicalResult
	webCalls := map[string]map[string]any{}
	var textParts []string
	var annotations []any
	flushText := func() {
		if len(textParts) == 0 {
			return
		}
		result.Output = append(result.Output, messageItem(strings.Join(textParts, "\n"), annotations))
		textParts = nil
		annotations = nil
	}
	for _, raw := range anySlice(root["content"]) {
		block, _ := raw.(map[string]any)
		typ := stringValue(block["type"])
		if typ != "text" {
			flushText()
		}
		switch typ {
		case "thinking":
			if text := stringValue(block["thinking"]); text != "" {
				result.Output = append(result.Output, reasoningItem(text, stringValue(block["signature"])))
			}
		case "redacted_thinking":
			result.Output = append(result.Output, reasoningItem("", stringValue(block["data"])))
		case "server_tool_use":
			if stringValue(block["name"]) != "web_search" {
				continue
			}
			input, _ := block["input"].(map[string]any)
			query := firstString(input, "query", "q")
			item := webSearchItem(firstString(block, "id"), query, nil, "completed")
			result.Output = append(result.Output, item)
			webCalls[firstString(block, "id")] = item
		case "web_search_tool_result":
			call := webCalls[firstString(block, "tool_use_id")]
			if call == nil {
				call = webSearchItem(firstString(block, "tool_use_id"), "", nil, "completed")
				result.Output = append(result.Output, call)
			}
			sources, failed := messageSearchSources(block["content"])
			action, _ := call["action"].(map[string]any)
			action["sources"] = sources
			if failed {
				call["status"] = "failed"
			}
		case "tool_use":
			args, _ := json.Marshal(valueOr(block["input"], map[string]any{}))
			result.Output = append(result.Output, functionCallItem(firstString(block, "id"), stringValue(block["name"]), string(args)))
		case "text":
			textParts = append(textParts, stringValue(block["text"]))
			annotations = append(annotations, citationsToAnnotations(block["citations"])...)
		}
	}
	flushText()
	if usage, _ := root["usage"].(map[string]any); usage != nil {
		result.InputTokens = numberInt(usage["input_tokens"])
		result.CachedTokens = numberInt(usage["cache_read_input_tokens"])
		result.OutputTokens = numberInt(usage["output_tokens"])
	}
	if stringValue(root["stop_reason"]) == "max_tokens" {
		result.IncompleteReason = "max_output_tokens"
	}
	return result, nil
}

func canonicalFromChat(data []byte, hosted bool, query string) (canonicalResult, error) {
	root, err := decodeJSONMap(data)
	if err != nil {
		return canonicalResult{}, err
	}
	var result canonicalResult
	choices := anySlice(root["choices"])
	if len(choices) == 0 {
		return result, fmt.Errorf("chat completions response has no choices")
	}
	choice, _ := choices[0].(map[string]any)
	message, _ := choice["message"].(map[string]any)
	if message == nil {
		return result, fmt.Errorf("chat completions response has no message")
	}
	if reasoning := firstString(message, "reasoning_content", "reasoning"); reasoning != "" {
		result.Output = append(result.Output, reasoningItem(reasoning, ""))
	}
	urls := collectCitationURLs(root, choice, message)
	if hosted && chatSearchExecuted(root, choice, message, urls) {
		result.Output = append(result.Output, webSearchItem("", query, urlsToSources(urls), "completed"))
	}
	for _, raw := range anySlice(message["tool_calls"]) {
		call, _ := raw.(map[string]any)
		fn, _ := call["function"].(map[string]any)
		args := stringValue(fn["arguments"])
		if args == "" && fn["arguments"] != nil {
			encoded, _ := json.Marshal(fn["arguments"])
			args = string(encoded)
		}
		result.Output = append(result.Output, functionCallItem(firstString(call, "id"), stringValue(fn["name"]), args))
	}
	if text := chatMessageText(message["content"]); text != "" {
		result.Output = append(result.Output, messageItem(text, urlsToAnnotations(urls)))
	}
	if usage, _ := root["usage"].(map[string]any); usage != nil {
		result.InputTokens = numberInt(valueFirst(usage, "prompt_tokens", "input_tokens"))
		result.OutputTokens = numberInt(valueFirst(usage, "completion_tokens", "output_tokens"))
		if details, _ := usage["prompt_tokens_details"].(map[string]any); details != nil {
			result.CachedTokens = numberInt(details["cached_tokens"])
		}
		if details, _ := usage["completion_tokens_details"].(map[string]any); details != nil {
			result.ReasoningTokens = numberInt(details["reasoning_tokens"])
		}
	}
	if finish := stringValue(choice["finish_reason"]); finish == "length" {
		result.IncompleteReason = "max_output_tokens"
	}
	return result, nil
}

// Chat Completions has no standard backend-tool-call item. Do not claim that a
// search ran merely because the request offered a search extension: require an
// upstream citation/result or an explicit server-side search usage counter.
func chatSearchExecuted(root, choice, message map[string]any, urls []string) bool {
	if len(urls) > 0 {
		return true
	}
	for _, values := range []map[string]any{root, choice, message} {
		for _, key := range []string{"search_results", "web_search_results", "sources", "citations", "annotations"} {
			if nonEmptyJSONValue(values[key]) {
				return true
			}
		}
	}
	usage, _ := root["usage"].(map[string]any)
	return positiveSearchUsage(usage)
}

func positiveSearchUsage(value any) bool {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			switch strings.ToLower(key) {
			case "num_sources_used", "web_search_requests", "search_requests", "search_queries_count":
				if numberInt(child) > 0 {
					return true
				}
			}
			if positiveSearchUsage(child) {
				return true
			}
		}
	case []any:
		for _, child := range typed {
			if positiveSearchUsage(child) {
				return true
			}
		}
	}
	return false
}

func nonEmptyJSONValue(value any) bool {
	switch typed := value.(type) {
	case []any:
		return len(typed) > 0
	case map[string]any:
		return len(typed) > 0
	case string:
		return strings.TrimSpace(typed) != ""
	default:
		return false
	}
}

func canonicalResponse(route config.Route, request facadeRequest, result canonicalResult) map[string]any {
	now := time.Now().Unix()
	status := "completed"
	var incomplete any
	if result.IncompleteReason != "" {
		status = "incomplete"
		incomplete = map[string]any{"reason": result.IncompleteReason}
	}
	total := result.InputTokens + result.OutputTokens
	return map[string]any{
		"id":                     compatID("resp"),
		"object":                 "response",
		"created_at":             now,
		"completed_at":           now,
		"status":                 status,
		"background":             false,
		"error":                  nil,
		"incomplete_details":     incomplete,
		"instructions":           nil,
		"max_output_tokens":      nil,
		"max_tool_calls":         nil,
		"metadata":               map[string]any{},
		"model":                  route.WireModel,
		"output":                 result.Output,
		"parallel_tool_calls":    true,
		"previous_response_id":   nil,
		"prompt_cache_key":       nil,
		"prompt_cache_retention": nil,
		"reasoning":              map[string]any{"effort": nil, "summary": nil},
		"safety_identifier":      nil,
		"service_tier":           "default",
		"store":                  false,
		"temperature":            1,
		"text":                   map[string]any{"format": map[string]any{"type": "text"}, "verbosity": nil},
		"tool_choice":            "auto",
		"tools":                  responseTools(request.HostedWebSearch),
		"top_logprobs":           0,
		"top_p":                  1,
		"truncation":             "disabled",
		"usage": map[string]any{
			"input_tokens":  result.InputTokens,
			"output_tokens": result.OutputTokens,
			"total_tokens":  total,
			"input_tokens_details": map[string]any{
				"cached_tokens": result.CachedTokens,
			},
			"output_tokens_details": map[string]any{
				"reasoning_tokens": result.ReasoningTokens,
			},
		},
		"user": nil,
	}
}

func responseTools(hosted bool) []any {
	if !hosted {
		return []any{}
	}
	return []any{map[string]any{"type": "web_search", "search_context_size": nil, "user_location": nil}}
}

func writeCanonicalResponse(w http.ResponseWriter, response map[string]any, stream bool) error {
	if response == nil {
		return fmt.Errorf("response body must be a JSON object")
	}
	if _, err := json.Marshal(response); err != nil {
		return fmt.Errorf("encode response: %w", err)
	}
	if !stream {
		data, err := json.Marshal(response)
		if err != nil {
			return fmt.Errorf("encode response: %w", err)
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(data)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
		return nil
	}
	output, ok := response["output"].([]any)
	if !ok {
		return fmt.Errorf("response output must be an array")
	}
	for index, raw := range output {
		item, ok := raw.(map[string]any)
		if !ok || item == nil {
			return fmt.Errorf("response output[%d] must be an object", index)
		}
		switch stringValue(item["type"]) {
		case "message", "reasoning":
			content, ok := item["content"].([]any)
			if !ok {
				return fmt.Errorf("response output[%d].content must be an array", index)
			}
			for contentIndex, rawPart := range content {
				part, ok := rawPart.(map[string]any)
				if !ok || part == nil {
					return fmt.Errorf("response output[%d].content[%d] must be an object", index, contentIndex)
				}
			}
		}
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	sequence := 0
	emit := func(typ string, values map[string]any) {
		values["type"] = typ
		values["sequence_number"] = sequence
		sequence++
		data, _ := json.Marshal(values)
		_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", typ, data)
		if flusher != nil {
			flusher.Flush()
		}
	}
	created := cloneMap(response)
	created["completed_at"] = nil
	created["status"] = "in_progress"
	created["output"] = []any{}
	created["usage"] = nil
	emit("response.created", map[string]any{"response": created})
	emit("response.in_progress", map[string]any{"response": cloneMap(created)})

	for index, raw := range output {
		item := raw.(map[string]any)
		emitOutputItem(emit, index, item)
	}
	terminal := "response.completed"
	if stringValue(response["status"]) == "incomplete" {
		terminal = "response.incomplete"
	}
	emit(terminal, map[string]any{"response": response})
	return nil
}

func emitOutputItem(emit func(string, map[string]any), index int, item map[string]any) {
	typ := stringValue(item["type"])
	id := stringValue(item["id"])
	added := cloneMap(item)
	added["status"] = "in_progress"
	switch typ {
	case "reasoning":
		added["content"] = []any{}
		emit("response.output_item.added", map[string]any{"output_index": index, "item": added})
		for contentIndex, rawPart := range anySlice(item["content"]) {
			part, _ := rawPart.(map[string]any)
			text := stringValue(part["text"])
			emit("response.content_part.added", map[string]any{"output_index": index, "item_id": id, "content_index": contentIndex, "part": map[string]any{"type": "reasoning_text", "text": ""}})
			if text != "" {
				emit("response.reasoning_text.delta", map[string]any{"output_index": index, "item_id": id, "content_index": contentIndex, "delta": text})
			}
			emit("response.reasoning_text.done", map[string]any{"output_index": index, "item_id": id, "content_index": contentIndex, "text": text})
			emit("response.content_part.done", map[string]any{"output_index": index, "item_id": id, "content_index": contentIndex, "part": part})
		}
		emit("response.output_item.done", map[string]any{"output_index": index, "item": item})
	case "web_search_call":
		emit("response.output_item.added", map[string]any{"output_index": index, "item": added})
		emit("response.web_search_call.in_progress", map[string]any{"output_index": index, "item_id": id})
		emit("response.web_search_call.searching", map[string]any{"output_index": index, "item_id": id})
		emit("response.web_search_call.completed", map[string]any{"output_index": index, "item_id": id})
		emit("response.output_item.done", map[string]any{"output_index": index, "item": item})
	case "function_call":
		arguments := stringValue(item["arguments"])
		added["arguments"] = ""
		emit("response.output_item.added", map[string]any{"output_index": index, "item": added})
		if arguments != "" {
			emit("response.function_call_arguments.delta", map[string]any{"output_index": index, "item_id": id, "delta": arguments})
		}
		emit("response.function_call_arguments.done", map[string]any{"output_index": index, "item_id": id, "arguments": arguments})
		emit("response.output_item.done", map[string]any{"output_index": index, "item": item})
	case "message":
		added["content"] = []any{}
		emit("response.output_item.added", map[string]any{"output_index": index, "item": added})
		for contentIndex, rawPart := range anySlice(item["content"]) {
			part, _ := rawPart.(map[string]any)
			emptyPart := cloneMap(part)
			switch stringValue(part["type"]) {
			case "refusal":
				refusal := stringValue(part["refusal"])
				emptyPart["refusal"] = ""
				emit("response.content_part.added", map[string]any{"output_index": index, "item_id": id, "content_index": contentIndex, "part": emptyPart})
				if refusal != "" {
					emit("response.refusal.delta", map[string]any{"output_index": index, "item_id": id, "content_index": contentIndex, "delta": refusal})
				}
				emit("response.refusal.done", map[string]any{"output_index": index, "item_id": id, "content_index": contentIndex, "refusal": refusal})
			default:
				text := stringValue(part["text"])
				emptyPart["text"] = ""
				emit("response.content_part.added", map[string]any{"output_index": index, "item_id": id, "content_index": contentIndex, "part": emptyPart})
				if text != "" {
					emit("response.output_text.delta", map[string]any{"output_index": index, "item_id": id, "content_index": contentIndex, "delta": text, "logprobs": []any{}})
				}
				emit("response.output_text.done", map[string]any{"output_index": index, "item_id": id, "content_index": contentIndex, "text": text, "logprobs": []any{}})
			}
			emit("response.content_part.done", map[string]any{"output_index": index, "item_id": id, "content_index": contentIndex, "part": part})
		}
		emit("response.output_item.done", map[string]any{"output_index": index, "item": item})
	default:
		emit("response.output_item.added", map[string]any{"output_index": index, "item": added})
		emit("response.output_item.done", map[string]any{"output_index": index, "item": item})
	}
}

func reasoningItem(text, encrypted string) map[string]any {
	item := map[string]any{"type": "reasoning", "id": compatID("rs"), "status": "completed", "summary": []any{}, "content": []any{}}
	if text != "" {
		item["content"] = []any{map[string]any{"type": "reasoning_text", "text": text}}
	}
	if encrypted != "" {
		item["encrypted_content"] = encrypted
	}
	return item
}

func messageItem(text string, annotations []any) map[string]any {
	if annotations == nil {
		annotations = []any{}
	}
	return map[string]any{
		"type": "message", "id": compatID("msg"), "status": "completed", "role": "assistant",
		"content": []any{map[string]any{"type": "output_text", "text": text, "annotations": annotations, "logprobs": []any{}}},
	}
}

func functionCallItem(id, name, arguments string) map[string]any {
	if id == "" {
		id = compatID("call")
	}
	return map[string]any{"type": "function_call", "id": compatID("fc"), "call_id": id, "name": name, "arguments": arguments, "status": "completed"}
}

func webSearchItem(id, query string, sources []any, status string) map[string]any {
	if id == "" {
		id = compatID("ws")
	}
	if sources == nil {
		sources = []any{}
	}
	return map[string]any{"type": "web_search_call", "id": id, "status": status, "action": map[string]any{"type": "search", "query": query, "sources": sources}}
}

func messageSearchSources(value any) ([]any, bool) {
	var sources []any
	failed := false
	if errorBlock, _ := value.(map[string]any); errorBlock != nil {
		failed = strings.Contains(stringValue(errorBlock["type"]), "error")
	}
	for _, raw := range anySlice(value) {
		entry, _ := raw.(map[string]any)
		if stringValue(entry["type"]) != "web_search_result" {
			continue
		}
		if rawURL := stringValue(entry["url"]); rawURL != "" {
			source := map[string]any{"type": "url", "url": rawURL}
			if title := stringValue(entry["title"]); title != "" {
				source["title"] = title
			}
			sources = append(sources, source)
		}
	}
	return sources, failed
}

func chatMessageText(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	var parts []string
	for _, raw := range anySlice(value) {
		part, _ := raw.(map[string]any)
		if typ := stringValue(part["type"]); typ == "text" || typ == "output_text" {
			parts = append(parts, stringValue(part["text"]))
		}
	}
	return strings.Join(parts, "")
}

func collectCitationURLs(values ...map[string]any) []string {
	seen := map[string]bool{}
	var urls []string
	var walk func(any)
	walk = func(value any) {
		switch v := value.(type) {
		case map[string]any:
			for key, child := range v {
				if (key == "url" || key == "uri") && stringValue(child) != "" {
					u := stringValue(child)
					if strings.HasPrefix(u, "http") && !seen[u] {
						seen[u] = true
						urls = append(urls, u)
					}
				}
				if key == "citations" || key == "annotations" || key == "search_results" || key == "sources" {
					walk(child)
				}
			}
		case []any:
			for _, child := range v {
				walk(child)
			}
		case string:
			if strings.HasPrefix(v, "http") && !seen[v] {
				seen[v] = true
				urls = append(urls, v)
			}
		}
	}
	for _, value := range values {
		walk(value)
	}
	sort.Strings(urls)
	return urls
}

func citationsToAnnotations(value any) []any {
	var root map[string]any
	if value != nil {
		root = map[string]any{"citations": value}
	}
	return urlsToAnnotations(collectCitationURLs(root))
}

func urlsToAnnotations(urls []string) []any {
	annotations := make([]any, 0, len(urls))
	for _, rawURL := range urls {
		annotations = append(annotations, map[string]any{"type": "url_citation", "url": rawURL, "title": rawURL, "start_index": 0, "end_index": 0})
	}
	return annotations
}

func urlsToSources(urls []string) []any {
	sources := make([]any, 0, len(urls))
	for _, rawURL := range urls {
		sources = append(sources, map[string]any{"type": "url", "url": rawURL})
	}
	return sources
}

func decodeJSONMap(data []byte) (map[string]any, error) {
	var root map[string]any
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&root); err != nil {
		return nil, err
	}
	if root == nil {
		return nil, fmt.Errorf("response body must be a JSON object")
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("multiple JSON values")
		}
		return nil, err
	}
	return root, nil
}

func numberInt(value any) int {
	return positiveInt(value, 0)
}

func valueFirst(values map[string]any, keys ...string) any {
	for _, key := range keys {
		if value, ok := values[key]; ok && value != nil {
			return value
		}
	}
	return nil
}

func cloneMap(value map[string]any) map[string]any {
	data, _ := json.Marshal(value)
	var clone map[string]any
	_ = json.Unmarshal(data, &clone)
	return clone
}

func compatID(prefix string) string {
	var data [12]byte
	_, _ = rand.Read(data[:])
	return prefix + "_" + hex.EncodeToString(data[:])
}
