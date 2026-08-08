package proxy

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/hellowind777/hellogrok/internal/config"
)

func TestFacadeEnsuresHostedSearchUnlessToolsAreDisabled(t *testing.T) {
	route := config.Route{
		ChannelID:             "third-party",
		APIBackend:            "responses",
		WireModel:             "model-real",
		SupportsBackendSearch: true,
	}
	request, err := adaptFacadeRequest([]byte(`{"model":"other","input":"hi"}`), route, newSearchReplayCache())
	if err != nil {
		t.Fatal(err)
	}
	_, _, hosted, function, xSearch := summarizeBody(request.Body)
	if !request.HostedWebSearch || hosted != 1 || function != 0 || xSearch != 0 ||
		request.BuildHostedWebSearch != 0 || request.BuildXSearch != 0 || !request.ProxyAddedWebSearch {
		t.Fatalf("hosted search was not ensured: %s", request.Body)
	}

	disabled, err := adaptFacadeRequest([]byte(`{"model":"other","input":"hi","tool_choice":"none"}`), route, newSearchReplayCache())
	if err != nil {
		t.Fatal(err)
	}
	_, _, hosted, function, xSearch = summarizeBody(disabled.Body)
	if disabled.HostedWebSearch || hosted != 0 || function != 0 || xSearch != 0 {
		t.Fatalf("tool_choice=none was ignored: %s", disabled.Body)
	}
}

func TestFacadeRecordsBuildXSearchNormalization(t *testing.T) {
	route := config.Route{
		ChannelID:             "third-party",
		APIBackend:            "responses",
		WireModel:             "model-real",
		SupportsBackendSearch: true,
	}
	request, err := adaptFacadeRequest([]byte(`{"input":"search","tools":[{"type":"x_search"}]}`), route, newSearchReplayCache())
	if err != nil {
		t.Fatal(err)
	}
	_, _, hosted, _, xSearch := summarizeBody(request.Body)
	if request.BuildXSearch != 1 || request.BuildHostedWebSearch != 0 || request.ProxyAddedWebSearch ||
		hosted != 1 || xSearch != 0 {
		t.Fatalf("Build x_search origin was not retained: request=%+v body=%s", request, request.Body)
	}
}

func TestFacadeUsesProviderSpecificHostedSearchDialect(t *testing.T) {
	base := config.Route{APIBackend: "responses", SupportsBackendSearch: true}
	for _, test := range []struct {
		name        string
		route       config.Route
		wantXSearch int
	}{
		{name: "Grok relay", route: config.Route{ChannelID: "relay", WireModel: "grok-4.5"}, wantXSearch: 1},
		{name: "provider-qualified Grok", route: config.Route{ChannelID: "relay", WireModel: "Console/grok-4.5"}, wantXSearch: 1},
		{name: "generic Responses", route: config.Route{ChannelID: "gpt-relay", WireModel: "gpt-5.6"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			route := base
			route.ChannelID = test.route.ChannelID
			route.WireModel = test.route.WireModel
			request, err := adaptFacadeRequest([]byte(`{"input":"search"}`), route, newSearchReplayCache())
			if err != nil {
				t.Fatal(err)
			}
			_, _, hosted, function, xSearch := summarizeBody(request.Body)
			if hosted != 1 || function != 0 || xSearch != test.wantXSearch {
				t.Fatalf("wrong dialect hosted=%d function=%d x=%d body=%s", hosted, function, xSearch, request.Body)
			}
		})
	}
}

func TestFacadePreservesClientSearchOnEveryUpstreamProtocol(t *testing.T) {
	body := []byte(`{
		"input":"search then fetch",
		"tools":[
			{"type":"function","name":"web_search","description":"Search","parameters":{"type":"object"}},
			{"type":"function","name":"web_fetch","description":"Fetch","parameters":{"type":"object"}}
		]
	}`)
	for _, backend := range []string{"responses", "messages", "chat_completions"} {
		t.Run(backend, func(t *testing.T) {
			request, err := adaptFacadeRequest(body, config.Route{
				ChannelID:  "client-search-" + backend,
				APIBackend: backend,
				WireModel:  "model-real",
			}, newSearchReplayCache())
			if err != nil {
				t.Fatal(err)
			}
			_, _, hosted, function, xSearch := summarizeBody(request.Body)
			if request.HostedWebSearch || request.ProxyAddedWebSearch || hosted != 0 || function != 1 || xSearch != 0 {
				t.Fatalf("client search was changed for %s: request=%+v body=%s", backend, request, request.Body)
			}
			if !strings.Contains(string(request.Body), `"web_fetch"`) {
				t.Fatalf("web_fetch was lost for %s: %s", backend, request.Body)
			}
		})
	}
}

func TestFacadePreservesStructuredClientSearchChoiceOnEveryProtocol(t *testing.T) {
	body := []byte(`{
		"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"Use web_search to find the latest Grok Build release."}]}],
		"tool_choice":{"type":"function","name":"web_search"},
		"tools":[
			{"type":"function","name":"web_search","description":"Search","parameters":{"type":"object"}},
			{"type":"function","name":"web_fetch","description":"Fetch","parameters":{"type":"object"}}
		]
	}`)
	for _, backend := range []string{"responses", "messages", "chat_completions"} {
		t.Run(backend, func(t *testing.T) {
			request, err := adaptFacadeRequest(body, config.Route{
				ChannelID:  "client-search-" + backend,
				APIBackend: backend,
				WireModel:  "model-real",
			}, newSearchReplayCache())
			if err != nil {
				t.Fatal(err)
			}
			if request.HostedWebSearch || request.ClientSearchAlias == "" {
				t.Fatalf("structured client search choice was not preserved for %s: request=%+v body=%s", backend, request, request.Body)
			}

			root, err := decodeRequestObject(request.Body)
			if err != nil {
				t.Fatal(err)
			}
			choice, _ := root["tool_choice"].(map[string]any)
			switch backend {
			case "responses":
				if stringValue(choice["type"]) != "function" || stringValue(choice["name"]) != request.ClientSearchAlias {
					t.Fatalf("wrong Responses tool choice: %#v", root["tool_choice"])
				}
			case "messages":
				if stringValue(choice["type"]) != "tool" || stringValue(choice["name"]) != request.ClientSearchAlias {
					t.Fatalf("wrong Messages tool choice: %#v", root["tool_choice"])
				}
			case "chat_completions":
				function, _ := choice["function"].(map[string]any)
				if stringValue(choice["type"]) != "function" || stringValue(function["name"]) != request.ClientSearchAlias {
					t.Fatalf("wrong Chat tool choice: %#v", root["tool_choice"])
				}
			}
			if !strings.Contains(string(request.Body), "configured client web-search model") ||
				!strings.Contains(string(request.Body), "refer to this tool only as web_search") ||
				!strings.Contains(string(request.Body), "Do not use this tool to search") ||
				!strings.Contains(string(request.Body), "web_fetch") {
				t.Fatalf("client tool guidance was not preserved for %s: %s", backend, request.Body)
			}
		})
	}
}

func TestFacadeDoesNotInferClientSearchChoiceFromPrompt(t *testing.T) {
	for _, prompt := range []string{
		"Use web_search to find the latest release.",
		"不要用网络搜索，直接回答。",
		"Call spawn_subagent and ask the child to use web_search.",
	} {
		body, err := json.Marshal(map[string]any{
			"input":       prompt,
			"tool_choice": "auto",
			"tools": []any{
				map[string]any{"type": "function", "name": "web_search", "parameters": map[string]any{"type": "object"}},
				map[string]any{"type": "function", "name": "web_fetch", "parameters": map[string]any{"type": "object"}},
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		request, err := adaptFacadeRequest(body, config.Route{
			ChannelID:  "client-search",
			APIBackend: "responses",
			WireModel:  "model-real",
		}, newSearchReplayCache())
		if err != nil {
			t.Fatal(err)
		}
		root, err := decodeRequestObject(request.Body)
		if err != nil {
			t.Fatal(err)
		}
		if root["tool_choice"] != "auto" {
			t.Fatalf("prompt changed structured tool choice for %q: %#v", prompt, root["tool_choice"])
		}
		if request.ClientSearchAlias == "" || !strings.Contains(string(request.Body), "configured client web-search model") {
			t.Fatalf("client search tool was not described and aliased: %s", request.Body)
		}
	}
}

func TestConvertedProtocolsEnforceAllowedToolsChoice(t *testing.T) {
	for _, backend := range []string{"messages", "chat_completions"} {
		t.Run(backend, func(t *testing.T) {
			request, err := adaptFacadeRequest([]byte(`{
				"input":"do work",
				"tools":[
					{"type":"function","name":"save","parameters":{"type":"object"}},
					{"type":"function","name":"delete","parameters":{"type":"object"}},
					{"type":"web_search"}
				],
				"tool_choice":{"type":"allowed_tools","mode":"required","tools":[{"type":"function","name":"save"}]}
			}`), config.Route{
				ChannelID:             "allowed-" + backend,
				APIBackend:            backend,
				WireModel:             "model-real",
				SupportsBackendSearch: true,
			}, newSearchReplayCache())
			if err != nil {
				t.Fatal(err)
			}
			root, err := decodeRequestObject(request.Body)
			if err != nil {
				t.Fatal(err)
			}
			tools := anySlice(root["tools"])
			if len(tools) != 1 || strings.Contains(string(request.Body), `"delete"`) {
				t.Fatalf("disallowed function leaked to %s: %s", backend, request.Body)
			}
			if backend == "messages" {
				if tools[0].(map[string]any)["name"] != "save" || root["tool_choice"].(map[string]any)["type"] != "any" {
					t.Fatalf("Messages allowlist semantics changed: %s", request.Body)
				}
				return
			}
			function := tools[0].(map[string]any)["function"].(map[string]any)
			if function["name"] != "save" || root["tool_choice"] != "required" {
				t.Fatalf("Chat allowlist semantics changed: %s", request.Body)
			}
			if _, exists := root["web_search_options"]; exists {
				t.Fatalf("excluded hosted search leaked to Chat: %s", request.Body)
			}
		})
	}
}

func TestHostedSearchInjectionRespectsExplicitAllowedTools(t *testing.T) {
	request, err := adaptFacadeRequest([]byte(`{
		"input":"save this",
		"tools":[{"type":"function","name":"save","parameters":{"type":"object"}}],
		"tool_choice":{"type":"allowed_tools","mode":"auto","tools":[{"type":"function","name":"save"}]}
	}`), config.Route{
		ChannelID:             "responses-allowlist",
		APIBackend:            "responses",
		WireModel:             "model-real",
		SupportsBackendSearch: true,
	}, newSearchReplayCache())
	if err != nil {
		t.Fatal(err)
	}
	_, _, hosted, _, xSearch := summarizeBody(request.Body)
	if request.ProxyAddedWebSearch || hosted != 0 || xSearch != 0 {
		t.Fatalf("hosted search bypassed explicit allowed_tools: %s", request.Body)
	}
}

func TestMessagesConversionRejectsInvalidFunctionArguments(t *testing.T) {
	_, err := adaptFacadeRequest([]byte(`{
		"input":[{"type":"function_call","call_id":"call_1","name":"save","arguments":"{broken"}]
	}`), config.Route{ChannelID: "messages", APIBackend: "messages", WireModel: "model-real"}, newSearchReplayCache())
	if err == nil || !strings.Contains(err.Error(), "arguments must be one JSON object") {
		t.Fatalf("invalid Messages tool arguments were silently accepted: %v", err)
	}
}

func TestFacadePreparesBuildClientSearchExecution(t *testing.T) {
	request, err := adaptFacadeRequest([]byte(`{
		"model":"deepseek-v4-flash",
		"input":"latest Grok Build commit",
		"store":false,
		"temperature":0.1,
		"top_p":0.95,
		"max_output_tokens":8192,
		"tools":[{"type":"web_search","filters":{"allowed_domains":["github.com"]}}]
	}`), config.Route{
		ChannelID:  "deepseek-v4-flash",
		APIBackend: "responses",
		WireModel:  "deepseek-v4-flash",
	}, newSearchReplayCache())
	if err != nil {
		t.Fatal(err)
	}
	if !request.ClientSearchPrepared || request.ProxyAddedWebSearch {
		t.Fatalf("Build client-search execution was not recognized: %+v", request)
	}
	root, err := decodeRequestObject(request.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stringValue(root["instructions"]), "always return a concise final text synthesis") {
		t.Fatalf("search synthesis instruction missing: %s", request.Body)
	}
	choice, _ := root["tool_choice"].(map[string]any)
	if stringValue(choice["type"]) != "web_search" {
		t.Fatalf("hosted search was not selected: %#v", root["tool_choice"])
	}
	_, _, hosted, function, xSearch := summarizeBody(request.Body)
	if hosted != 1 || function != 0 || xSearch != 0 {
		t.Fatalf("client-search execution gained the conversation x_search policy: %s", request.Body)
	}
}

func TestFacadeDoesNotPrepareOrdinaryHostedRequests(t *testing.T) {
	route := config.Route{
		ChannelID:             "hosted",
		APIBackend:            "responses",
		WireModel:             "gpt-search",
		SupportsBackendSearch: true,
	}
	for _, test := range []struct {
		name string
		body string
	}{
		{name: "streaming conversation", body: `{"input":"search","store":false,"stream":true,"tools":[{"type":"web_search"}]}`},
		{name: "structured conversation", body: `{"input":[{"type":"message","role":"user","content":"search"}],"store":false,"tools":[{"type":"web_search"}]}`},
		{name: "proxy-added hosted tool", body: `{"input":"hi","store":false}`},
		{name: "mixed tools", body: `{"input":"search","store":false,"tools":[{"type":"web_search"},{"type":"function","name":"save","parameters":{"type":"object"}}]}`},
		{name: "ordinary non-stream request", body: `{"input":"hi","store":false,"temperature":0.1,"top_p":0.95,"max_output_tokens":2048,"tools":[{"type":"web_search"}]}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			request, err := adaptFacadeRequest([]byte(test.body), route, newSearchReplayCache())
			if err != nil {
				t.Fatal(err)
			}
			if request.ClientSearchPrepared {
				t.Fatalf("ordinary hosted request was mistaken for Build client search: %s", request.Body)
			}
		})
	}
}

func TestFacadeDoesNotInventSearchForDisabledRoute(t *testing.T) {
	request, err := adaptFacadeRequest([]byte(`{
		"input":"hi",
		"tools":[{"type":"function","name":"web_fetch","parameters":{"type":"object"}}]
	}`), config.Route{
		ChannelID:  "no-search-model",
		APIBackend: "responses",
		WireModel:  "model-real",
	}, newSearchReplayCache())
	if err != nil {
		t.Fatal(err)
	}
	_, _, hosted, function, xSearch := summarizeBody(request.Body)
	if request.HostedWebSearch || request.ProxyAddedWebSearch || hosted != 0 || function != 0 || xSearch != 0 {
		t.Fatalf("disabled route gained search: request=%+v body=%s", request, request.Body)
	}
	if !strings.Contains(string(request.Body), `"web_fetch"`) {
		t.Fatalf("independent web_fetch was lost: %s", request.Body)
	}
}

func TestFacadeDropsUnrecognizedHostedSearchWhenBackendSearchIsDisabled(t *testing.T) {
	for _, test := range []struct {
		name  string
		model string
		body  string
	}{
		{name: "generic hosted request", model: "gpt-search", body: `{"input":"search","tools":[{"type":"function","name":"web_fetch","parameters":{}},{"type":"web_search"}],"tool_choice":{"type":"web_search"}}`},
		{name: "Grok x-search request", model: "grok-4.5", body: `{"input":"search","tools":[{"type":"function","name":"web_fetch","parameters":{}},{"type":"x_search"}],"tool_choice":{"type":"x_search"}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			request, err := adaptFacadeRequest([]byte(test.body), config.Route{
				ChannelID:  "search-model",
				APIBackend: "responses",
				WireModel:  test.model,
			}, newSearchReplayCache())
			if err != nil {
				t.Fatal(err)
			}
			_, _, hosted, function, xSearch := summarizeBody(request.Body)
			if request.HostedWebSearch || request.ProxyAddedWebSearch || hosted != 0 || function != 0 || xSearch != 0 {
				t.Fatalf("disabled route retained a hosted search declaration: request=%+v body=%s", request, request.Body)
			}
			root, err := decodeRequestObject(request.Body)
			if err != nil {
				t.Fatal(err)
			}
			if _, exists := root["tool_choice"]; exists || !strings.Contains(string(request.Body), `"web_fetch"`) {
				t.Fatalf("hosted tool choice was not repaired or web_fetch was lost: %s", request.Body)
			}
		})
	}
}

func TestChatReplayPreservesReasoningOrderingAndToolResultImages(t *testing.T) {
	input := []any{
		map[string]any{"type": "reasoning", "content": []any{map[string]any{"type": "reasoning_text", "text": "think"}}},
		map[string]any{"type": "web_search_call", "id": "ws_1", "status": "completed", "action": map[string]any{"type": "search", "query": "q"}},
		map[string]any{"type": "function_call", "call_id": "call_1", "name": "save", "arguments": `{"ok":true}`},
		map[string]any{"type": "function_call_output", "call_id": "call_1", "output": []any{
			map[string]any{"type": "input_text", "text": "saved"},
			map[string]any{"type": "input_image", "image_url": "https://example.test/image.png"},
		}},
	}
	messages, _, err := responsesInputToMessages(input, false, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 3 {
		t.Fatalf("messages=%d want 3: %#v", len(messages), messages)
	}
	backend := messages[0].(map[string]any)
	if backend["content"] != "[backend web_search] search: q" {
		t.Fatalf("backend summary mismatch: %#v", backend)
	}
	assistant := messages[1].(map[string]any)
	if assistant["reasoning_content"] != "think" || len(anySlice(assistant["tool_calls"])) != 1 {
		t.Fatalf("reasoning/tool call did not fold onto the following assistant: %#v", assistant)
	}
	tool := messages[2].(map[string]any)
	content := anySlice(tool["content"])
	if tool["role"] != "tool" || len(content) != 2 {
		t.Fatalf("tool result content was lost: %#v", tool)
	}
	image := content[1].(map[string]any)
	if image["type"] != "image_url" {
		t.Fatalf("tool result image was not converted: %#v", image)
	}
}

func TestMessagesReplayPreservesThinkingBackendAndImageResult(t *testing.T) {
	cache := newSearchReplayCache()
	cache.captureMessages("deepseek", "", []byte(`{
		"content":[
			{"type":"server_tool_use","id":"ws_1","name":"web_search","input":{"query":"q"}},
			{"type":"web_search_tool_result","tool_use_id":"ws_1","content":[{"type":"web_search_result","url":"https://example.test","title":"Example","page_age":"today","encrypted_content":"opaque"}]}
		]
	}`))
	input := []any{
		map[string]any{"type": "reasoning", "content": []any{map[string]any{"type": "reasoning_text", "text": "think"}}, "encrypted_content": "sig"},
		map[string]any{"type": "web_search_call", "id": "ws_1", "action": map[string]any{
			"type": "search", "query": "q",
			"sources": []any{map[string]any{"type": "url", "url": "https://example.test", "title": "Example"}},
		}},
		map[string]any{"type": "function_call", "call_id": "call_1", "name": "save", "arguments": `{}`},
		map[string]any{"type": "function_call_output", "call_id": "call_1", "output": []any{
			map[string]any{"type": "input_image", "image_url": "data:image/png;base64,AAAA"},
		}},
	}
	messages, _, err := responsesInputToMessages(input, true, "deepseek", cache)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 {
		t.Fatalf("messages=%d want 2: %#v", len(messages), messages)
	}
	assistant := messages[0].(map[string]any)
	blocks := anySlice(assistant["content"])
	if len(blocks) != 4 || blocks[0].(map[string]any)["type"] != "thinking" ||
		blocks[1].(map[string]any)["type"] != "server_tool_use" ||
		blocks[2].(map[string]any)["type"] != "web_search_tool_result" ||
		blocks[3].(map[string]any)["type"] != "tool_use" {
		t.Fatalf("assistant replay mismatch: %#v", assistant)
	}
	searchResult := anySlice(blocks[2].(map[string]any)["content"])[0].(map[string]any)
	if searchResult["page_age"] != "today" || searchResult["encrypted_content"] != "opaque" {
		t.Fatalf("provider search result was not replayed exactly: %#v", searchResult)
	}
	result := messages[1].(map[string]any)
	resultBlocks := anySlice(result["content"])
	toolResult := resultBlocks[0].(map[string]any)
	images := anySlice(toolResult["content"])
	if len(images) != 1 || images[0].(map[string]any)["type"] != "image" {
		t.Fatalf("Anthropic image result mismatch: %#v", result)
	}
}

func TestRouteLoopbackDetectionCoversStandardIPForms(t *testing.T) {
	for _, host := range []string{"localhost", "LOCALHOST:8000", "127.0.0.1:8000", "127.0.0.2:8000", "[::1]:8000"} {
		if !routeIsLoopback(config.Route{Host: host}) {
			t.Fatalf("loopback host was rejected: %s", host)
		}
	}
	for _, host := range []string{"example.com", "192.0.2.1:8000", "[2001:db8::1]:8000"} {
		if routeIsLoopback(config.Route{Host: host}) {
			t.Fatalf("remote host was accepted as loopback: %s", host)
		}
	}
}

func TestRouteHeadersNeverLeakOAuth(t *testing.T) {
	incoming := http.Header{
		"Authorization":    []string{"Bearer oauth-session"},
		"Cookie":           []string{"session=secret"},
		"X-Xai-Token-Auth": []string{"oauth-token"},
		"X-Trace":          []string{"keep"},
	}
	forwarded := http.Header{}
	copySafeRequestHeaders(forwarded, incoming)
	applyRouteHeaders(forwarded, config.Route{APIKey: "channel-key", AuthScheme: "x_api_key"}, incoming)
	if forwarded.Get("Authorization") != "" || forwarded.Get("X-Api-Key") != "channel-key" {
		t.Fatalf("wrong channel auth: %#v", forwarded)
	}
	if forwarded.Get("Cookie") != "" || forwarded.Get("X-Xai-Token-Auth") != "" {
		t.Fatalf("session credential leaked: %#v", forwarded)
	}
	if forwarded.Get("X-Trace") != "keep" {
		t.Fatalf("safe header was lost: %#v", forwarded)
	}
}

func TestSafeResponseHeadersDropHopByHopAndConnectionNamedFields(t *testing.T) {
	source := http.Header{
		"Connection":         []string{"keep-alive, X-Upstream-Only"},
		"Keep-Alive":         []string{"timeout=5"},
		"X-Upstream-Only":    []string{"secret-hop"},
		"Proxy-Authenticate": []string{"Basic realm=proxy"},
		"Trailer":            []string{"X-Trailer"},
		"Upgrade":            []string{"websocket"},
		"Set-Cookie":         []string{"session=secret"},
		"Location":           []string{"https://redirect.example"},
		"X-Request-Id":       []string{"request-1"},
	}
	destination := http.Header{}
	copySafeResponseHeaders(destination, source)
	if len(destination) != 1 || destination.Get("X-Request-Id") != "request-1" {
		t.Fatalf("unsafe response headers survived: %#v", destination)
	}
}

func TestSafeUpstreamErrorRedactsURLSecrets(t *testing.T) {
	err := &url.Error{
		Op:  "Post",
		URL: "https://api.example/tenant/secret-path-token/responses?api_key=secret-query-token",
		Err: errors.New("connection refused"),
	}
	detail := safeUpstreamError(err)
	if strings.Contains(detail, "secret-path-token") || strings.Contains(detail, "secret-query-token") || strings.Contains(detail, "api_key") {
		t.Fatalf("upstream error leaked URL credentials: %s", detail)
	}
	if !strings.Contains(detail, "https://api.example/.../responses") || !strings.Contains(detail, "connection refused") {
		t.Fatalf("sanitized error lost useful context: %s", detail)
	}
}

func TestDeepSeekReplayCacheIsExactAndChannelScoped(t *testing.T) {
	cache := newSearchReplayCache()
	cache.captureJSON("one", replayConversationFingerprint([]any{}), []byte(`{"type":"response.completed","response":{"output":[{"type":"web_search_call","id":"ws_1","action":{"type":"search","query":"q","queries":["q","q latest","ws_call_id=ws_1"]}}]}}`))
	makeRoot := func() map[string]any {
		var root map[string]any
		_ = json.Unmarshal([]byte(`{"input":[{"type":"web_search_call","id":"ws_1","action":{"type":"search","query":"q"}}]}`), &root)
		return root
	}
	other := makeRoot()
	if cache.restore("two", other) {
		t.Fatal("replay crossed channel boundary")
	}
	root := makeRoot()
	if !cache.restore("one", root) {
		t.Fatal("replay was not restored")
	}
	item := anySlice(root["input"])[0].(map[string]any)
	action := item["action"].(map[string]any)
	queries := anySlice(action["queries"])
	if len(queries) != 3 || queries[1] != "q latest" {
		t.Fatalf("queries were not restored exactly: %#v", queries)
	}
}

func TestMessagesHostedSearchToolChoiceIsNative(t *testing.T) {
	root, err := decodeRequestObject([]byte(`{
		"model":"deepseek-v4-pro",
		"input":"search now",
		"tools":[
			{"type":"web_search"},
			{"type":"function","name":"save","parameters":{"type":"object"}}
		],
		"tool_choice":{"type":"web_search"}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	converted, err := responsesToMessagesRequest(root, "deepseek-pro", newSearchReplayCache())
	if err != nil {
		t.Fatal(err)
	}
	choice, _ := converted["tool_choice"].(map[string]any)
	if choice["type"] != "tool" || choice["name"] != "web_search" {
		t.Fatalf("hosted search choice was not converted: %#v", converted)
	}
	tools := anySlice(converted["tools"])
	if len(tools) != 2 || !containsMessagesHostedSearch(tools) {
		t.Fatalf("hosted search or ordinary function was lost: %#v", converted)
	}
}

func TestChatHostedSearchChoiceKeepsOrdinaryFunctions(t *testing.T) {
	root, err := decodeRequestObject([]byte(`{
		"model":"grok-real",
		"input":"search now",
		"tools":[
			{"type":"web_search"},
			{"type":"function","name":"save","parameters":{"type":"object"}},
			{"type":"function","name":"web_search","parameters":{"type":"object"}}
		],
		"tool_choice":{"type":"web_search"}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	converted, err := responsesToChatRequest(root, config.Route{ChannelID: "grok-chat", WireModel: "grok-real", Host: "gateway.test"})
	if err != nil {
		t.Fatal(err)
	}
	search, _ := converted["search_parameters"].(map[string]any)
	if search["mode"] != "on" {
		t.Fatalf("forced xAI search was not enabled: %#v", converted)
	}
	tools := anySlice(converted["tools"])
	if len(tools) != 1 {
		t.Fatalf("hosted collision removed the wrong functions: %#v", converted)
	}
	function, _ := tools[0].(map[string]any)["function"].(map[string]any)
	if function["name"] != "save" {
		t.Fatalf("ordinary function was not preserved: %#v", converted)
	}
	if _, exists := converted["tool_choice"]; exists {
		t.Fatalf("hosted choice leaked as an invalid Chat function choice: %#v", converted)
	}
}

func TestChatHostedSearchUsesDeterministicProviderDialect(t *testing.T) {
	for _, test := range []struct {
		name      string
		route     config.Route
		wantField string
	}{
		{name: "Grok relay", route: config.Route{ChannelID: "grok-chat", WireModel: "grok-real"}, wantField: "search_parameters"},
		{name: "generic relay", route: config.Route{ChannelID: "generic-chat", WireModel: "model-real"}, wantField: "web_search_options"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root, err := decodeRequestObject([]byte(`{
				"model":"configured",
				"input":"search now",
				"tools":[{"type":"web_search"}],
				"tool_choice":"required"
			}`))
			if err != nil {
				t.Fatal(err)
			}
			converted, err := responsesToChatRequest(root, test.route)
			if err != nil {
				t.Fatal(err)
			}
			if _, exists := converted[test.wantField]; !exists {
				t.Fatalf("%s dialect was not used: %#v", test.wantField, converted)
			}
			other := "search_parameters"
			if test.wantField == other {
				other = "web_search_options"
			}
			if _, exists := converted[other]; exists {
				t.Fatalf("both Chat search dialects were sent: %#v", converted)
			}
		})
	}
}
