package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/hellowind777/hellogrok/internal/appinfo"
)

type requestMeta struct {
	Timestamp            string `json:"timestamp"`
	Target               string `json:"target"`
	Model                string `json:"model"`
	Bytes                int    `json:"bytes"`
	Tools                int    `json:"tools"`
	WebSearch            int    `json:"web_search"`
	HostedWebSearch      int    `json:"hosted_web_search"`
	FunctionWebSearch    int    `json:"function_web_search"`
	XSearch              int    `json:"x_search"`
	BuildHostedWebSearch int    `json:"build_hosted_web_search"`
	BuildXSearch         int    `json:"build_x_search"`
	ProxyAddedWebSearch  bool   `json:"proxy_added_web_search"`
	ClientSearchForced   bool   `json:"client_web_search_forced"`
	ClientSearchPrepared bool   `json:"client_web_search_prepared"`
	ClientSearchAliased  bool   `json:"client_web_search_aliased"`
}

var requestMetaWriteMu sync.Mutex

type wireProtocol string

const (
	wireUnknown         wireProtocol = "unknown"
	wireResponses       wireProtocol = "responses"
	wireMessages        wireProtocol = "messages"
	wireChatCompletions wireProtocol = "chat_completions"
)

// saveLastRequestMeta persists only structural diagnostics. Request content,
// tool descriptions, credentials, and user prompts are never written.
func saveLastRequestMeta(target, model string, bodyBytes, tools, webSearch, hostedWebSearch, functionWebSearch, xSearch int, request facadeRequest) {
	requestMetaWriteMu.Lock()
	defer requestMetaWriteMu.Unlock()

	dir := appinfo.DataDir()
	_ = os.MkdirAll(dir, 0o700)
	purgeLegacyRequestDiagnostics()
	b, err := json.Marshal(requestMeta{
		Timestamp:            time.Now().UTC().Format(time.RFC3339Nano),
		Target:               safeDiagnosticTarget(target),
		Model:                model,
		Bytes:                bodyBytes,
		Tools:                tools,
		WebSearch:            webSearch,
		HostedWebSearch:      hostedWebSearch,
		FunctionWebSearch:    functionWebSearch,
		XSearch:              xSearch,
		BuildHostedWebSearch: request.BuildHostedWebSearch,
		BuildXSearch:         request.BuildXSearch,
		ProxyAddedWebSearch:  request.ProxyAddedWebSearch,
		ClientSearchForced:   request.ClientSearchForced,
		ClientSearchPrepared: request.ClientSearchPrepared,
		ClientSearchAliased:  request.ClientSearchAlias != "",
	})
	if err == nil {
		_ = writeRequestMetaAtomic(filepath.Join(dir, "last_request_meta.json"), b)
	}
}

func writeRequestMetaAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".hellogrok-request-meta-*")
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
		return err
	}
	committed = true
	return nil
}

func purgeLegacyRequestDiagnostics() {
	dir := appinfo.DataDir()
	_ = os.Remove(filepath.Join(dir, "last_request.json"))
	_ = os.Remove(filepath.Join(dir, "last_request_meta.txt"))
}

// summarizeBody reports only tool structure needed for compatibility diagnostics.
func summarizeBody(body []byte) (tools, webSearch, hostedWebSearch, functionWebSearch, xSearch int) {
	var m map[string]any
	if json.Unmarshal(body, &m) != nil {
		return 0, 0, 0, 0, 0
	}
	definitions, _ := m["tools"].([]any)
	tools = len(definitions)
	for _, field := range []string{"web_search_options", "search_parameters"} {
		if options, exists := m[field]; exists && options != nil {
			tools++
			webSearch++
			hostedWebSearch++
		}
	}
	for _, definition := range definitions {
		tool, ok := definition.(map[string]any)
		if !ok {
			continue
		}
		typ, _ := tool["type"].(string)
		if typ == "x_search" {
			xSearch++
		}
		name, _ := tool["name"].(string)
		if fn, ok := tool["function"].(map[string]any); ok && name == "" {
			name, _ = fn["name"].(string)
		}
		if typ == "web_search" || strings.HasPrefix(typ, "web_search_") {
			hostedWebSearch++
			webSearch++
		} else if name == "web_search" || isClientWebSearchWireAlias(name) {
			functionWebSearch++
			webSearch++
		}
	}
	return tools, webSearch, hostedWebSearch, functionWebSearch, xSearch
}

func decodeRequestObject(body []byte) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var root map[string]any
	if err := dec.Decode(&root); err != nil {
		return nil, err
	}
	if root == nil {
		return nil, fmt.Errorf("request body must be a JSON object")
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		return nil, fmt.Errorf("request body contains trailing JSON")
	}
	return root, nil
}

func encodeRequestObject(root map[string]any) ([]byte, error) {
	out, err := json.Marshal(root)
	if err != nil {
		return nil, err
	}
	return out, nil
}

func toolChoiceDisablesTools(choice any) bool {
	switch value := choice.(type) {
	case string:
		return value == "none"
	case map[string]any:
		typ, _ := value["type"].(string)
		return typ == "none"
	default:
		return false
	}
}

// normalizeHostedSearchRequest emits the hosted-search dialect expected by the
// selected upstream. Grok Build routes receive both of Build's native search
// declarations; other Responses providers receive only standard web_search.
func normalizeHostedSearchRequest(body []byte, grokRoute bool) ([]byte, bool, error) {
	root, err := decodeRequestObject(body)
	if err != nil {
		return body, false, err
	}
	changed := repairDeepSeekSearchReplay(root)

	tools, _ := root["tools"].([]any)
	hasHosted := false
	var canonicalWeb map[string]any
	var canonicalX map[string]any
	for _, entry := range tools {
		tool, _ := entry.(map[string]any)
		typ := stringValue(tool["type"])
		if isHostedWebSearchType(typ) {
			hasHosted = true
			if canonicalWeb == nil {
				canonicalWeb = cloneMap(tool)
				if stringValue(canonicalWeb["type"]) != "web_search" {
					canonicalWeb["type"] = "web_search"
					changed = true
				}
			}
		}
		if typ == "x_search" {
			hasHosted = true
			if canonicalX == nil {
				canonicalX = cloneMap(tool)
			}
		}
	}
	if canonicalWeb == nil && hasHosted {
		canonicalWeb = map[string]any{"type": "web_search"}
		changed = true
	}
	if grokRoute && canonicalX == nil && hasHosted {
		canonicalX = map[string]any{"type": "x_search"}
		changed = true
	}
	normalized := make([]any, 0, len(tools))
	hostedInserted := false
	for _, entry := range tools {
		tool, _ := entry.(map[string]any)
		typ := stringValue(tool["type"])
		if isHostedWebSearchType(typ) || typ == "x_search" {
			if !hostedInserted {
				normalized = append(normalized, canonicalWeb)
				if grokRoute {
					normalized = append(normalized, canonicalX)
				}
				hostedInserted = true
			}
			continue
		}
		if hasHosted && isSearchFunctionTool(tool) {
			changed = true
			continue
		}
		normalized = append(normalized, entry)
	}
	if hasHosted {
		if !hostedInserted {
			normalized = append(normalized, canonicalWeb)
			if grokRoute {
				normalized = append(normalized, canonicalX)
			}
		}
		if !sameJSONValue(tools, normalized) {
			root["tools"] = normalized
			changed = true
		}
	}
	if hasHosted && normalizeHostedToolChoice(root["tool_choice"], grokRoute) {
		changed = true
	}
	if !changed {
		return body, false, nil
	}
	out, err := encodeRequestObject(root)
	if err != nil {
		return body, false, err
	}
	return out, true, nil
}

// DeepSeek Responses returns a non-standard action.queries field and requires
// it when the completed web_search_call is replayed on the next turn. Build's
// standard Responses model drops that field while deserializing the response.
func repairDeepSeekSearchReplay(root map[string]any) bool {
	model, _ := root["model"].(string)
	if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(model)), "deepseek-") {
		return false
	}
	input, ok := root["input"].([]any)
	if !ok {
		return false
	}

	changed := false
	for _, entry := range input {
		item, _ := entry.(map[string]any)
		if typ, _ := item["type"].(string); typ != "web_search_call" {
			continue
		}
		action, _ := item["action"].(map[string]any)
		if typ, _ := action["type"].(string); typ != "search" {
			continue
		}
		if queries, exists := action["queries"]; exists && queries != nil {
			continue
		}
		query, _ := action["query"].(string)
		if query = strings.TrimSpace(query); query != "" {
			action["queries"] = []any{query}
		} else {
			action["queries"] = []any{}
		}
		changed = true
	}
	return changed
}

func isHostedWebSearchType(typ string) bool {
	typ = strings.ToLower(strings.TrimSpace(typ))
	return typ == "web_search" || strings.HasPrefix(typ, "web_search_")
}

func functionToolName(tool map[string]any) string {
	if stringValue(tool["type"]) != "function" {
		return ""
	}
	if name := stringValue(tool["name"]); name != "" {
		return name
	}
	function, _ := tool["function"].(map[string]any)
	return stringValue(function["name"])
}

func isSearchFunctionTool(tool map[string]any) bool {
	name := strings.ToLower(strings.TrimSpace(functionToolName(tool)))
	return name == "web_search" || name == "x_search"
}

func normalizeHostedToolChoice(choice any, grokRoute bool) bool {
	value, ok := choice.(map[string]any)
	if !ok {
		return false
	}
	typ := strings.ToLower(strings.TrimSpace(stringValue(value["type"])))
	if typ == "allowed_tools" {
		return normalizeAllowedHostedTools(value, grokRoute)
	}
	name := strings.ToLower(strings.TrimSpace(stringValue(value["name"])))
	if function, _ := value["function"].(map[string]any); name == "" && function != nil {
		name = strings.ToLower(strings.TrimSpace(stringValue(function["name"])))
	}
	if typ == "web_search" {
		return false
	}
	if grokRoute && typ == "x_search" {
		return false
	}
	if typ != "x_search" && !isHostedWebSearchType(typ) &&
		!(typ == "function" && (name == "web_search" || name == "x_search")) {
		return false
	}
	for key := range value {
		delete(value, key)
	}
	if grokRoute && name == "x_search" {
		value["type"] = "x_search"
	} else {
		value["type"] = "web_search"
	}
	return true
}

func normalizeAllowedHostedTools(choice map[string]any, grokRoute bool) bool {
	allowed, ok := choice["tools"].([]any)
	if !ok {
		return false
	}
	var normalized []any
	searchSeen := false
	for _, raw := range allowed {
		tool, _ := raw.(map[string]any)
		typ := strings.ToLower(strings.TrimSpace(stringValue(tool["type"])))
		name := strings.ToLower(strings.TrimSpace(functionToolName(tool)))
		isSearch := typ == "x_search" || isHostedWebSearchType(typ) ||
			(typ == "function" && (name == "web_search" || name == "x_search"))
		if !isSearch {
			normalized = append(normalized, raw)
			continue
		}
		if searchSeen {
			continue
		}
		searchSeen = true
		normalized = append(normalized, map[string]any{"type": "web_search"})
		if grokRoute {
			normalized = append(normalized, map[string]any{"type": "x_search"})
		}
	}
	if !searchSeen || sameJSONValue(allowed, normalized) {
		return false
	}
	choice["tools"] = normalized
	return true
}

func sameJSONValue(left, right any) bool {
	leftJSON, leftErr := json.Marshal(left)
	rightJSON, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftJSON, rightJSON)
}
