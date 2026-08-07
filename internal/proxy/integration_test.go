package proxy

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hellowind777/hellogrok/internal/config"
)

func startPathTestServer(t *testing.T, s *Server) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s.pathLn = ln
	s.PathAddr = ln.Addr().String()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		_ = http.Serve(ln, http.HandlerFunc(s.servePath))
	}()
	t.Cleanup(s.Stop)
}

func TestServerCanRestartAfterStop(t *testing.T) {
	s := New(log.New(io.Discard, "", 0))
	s.PathAddr = "127.0.0.1:0"
	if err := s.StartPath(); err != nil {
		t.Fatal(err)
	}
	s.Stop()
	if err := s.StartPath(); err != nil {
		t.Fatalf("restart failed: %v", err)
	}
	s.Stop()
}

func TestServerForcesActiveRequestsClosedAfterShutdownTimeout(t *testing.T) {
	entered := make(chan struct{})
	canceled := make(chan struct{})
	release := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		_ = r.Body.Close()
		close(entered)
		select {
		case <-r.Context().Done():
			close(canceled)
		case <-release:
		}
	}))
	defer up.Close()
	defer close(release)
	s := New(log.New(io.Discard, "", 0))
	s.shutdownTimeout = 25 * time.Millisecond
	s.PathAddr = "127.0.0.1:0"
	s.SetRoutes([]config.Route{facadeRoute("blocking", "responses", "m", "key", up.URL)})
	if err := s.StartPath(); err != nil {
		t.Fatal(err)
	}
	actualAddress := s.pathLn.Addr().String()
	requestDone := make(chan struct{})
	go func() {
		defer close(requestDone)
		request, _ := http.NewRequest(http.MethodPost, "http://"+actualAddress+"/c/blocking/responses", strings.NewReader(`{"input":"hi"}`))
		request.Header.Set("Content-Type", "application/json")
		response, err := http.DefaultClient.Do(request)
		if err == nil {
			_ = response.Body.Close()
		}
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("request never reached upstream")
	}
	s.connections.mu.Lock()
	trackedConnections := len(s.connections.conns)
	s.connections.mu.Unlock()
	if trackedConnections == 0 {
		t.Fatal("active upstream connection was not tracked")
	}
	s.Stop()
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("active upstream request survived server stop")
	}
	select {
	case <-requestDone:
	case <-time.After(time.Second):
		t.Fatal("local client remained blocked after server stop")
	}
}

func facadeRoute(id, backend, model, key, origin string) config.Route {
	host := strings.TrimPrefix(origin, "http://")
	if index := strings.IndexByte(host, '/'); index >= 0 {
		host = host[:index]
	}
	route := config.Route{
		ChannelID:             id,
		Host:                  host,
		OriginBase:            origin,
		APIBackend:            backend,
		WireModel:             model,
		APIKey:                key,
		SupportsBackendSearch: true,
	}
	if backend == "messages" {
		route.AuthScheme = "x_api_key"
	}
	return route
}

func postFacade(t *testing.T, s *Server, channel string, body []byte, auth string) ([]byte, int) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, "http://"+s.PathAddr+"/c/"+channel+"/responses", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return data, resp.StatusCode
}

func TestResponsesFacadeNormalizesSearchAndIsolatesAuth(t *testing.T) {
	var gotAuth, gotPath string
	var gotBody []byte
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath = r.Header.Get("Authorization"), r.URL.Path
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"type":"response.completed","response":{"id":"resp_1","object":"response","status":"completed","model":"gpt-real","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"OK"}]}],"usage":{"input_tokens":1,"output_tokens":1}}}`+"\n\n")
	}))
	defer up.Close()

	s := New(log.New(io.Discard, "", 0))
	s.SetRoutes([]config.Route{facadeRoute("gpt-channel", "responses", "gpt-real", "sk-channel", up.URL+"/v1")})
	startPathTestServer(t, s)
	body := []byte(`{"model":"grok-4.5","input":"hi","stream":true,"tool_choice":{"type":"x_search"},"tools":[{"type":"function","name":"x_search","parameters":{}},{"type":"web_search"},{"type":"x_search"}]}`)
	data, status := postFacade(t, s, "gpt-channel", body, "Bearer oauth-super-grok")
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, data)
	}
	if gotAuth != "Bearer sk-channel" || gotPath != "/v1/responses" {
		t.Fatalf("auth/path got %q %q", gotAuth, gotPath)
	}
	if extractModel(gotBody) != "gpt-real" {
		t.Fatalf("route model did not win: %s", gotBody)
	}
	tools, _, hosted, function, xSearch := summarizeBody(gotBody)
	if tools != 1 || hosted != 1 || function != 0 || xSearch != 0 {
		t.Fatalf("search tools not normalized/deduplicated: %s", gotBody)
	}
	if bytes.Contains(gotBody, []byte(`"name":"x_search"`)) || bytes.Contains(gotBody, []byte(`"name":"web_search"`)) {
		t.Fatalf("colliding client search function was not dropped: %s", gotBody)
	}
	if !bytes.Contains(data, []byte(`"annotations":[]`)) || !bytes.Contains(data, []byte(`"sequence_number"`)) {
		t.Fatalf("Responses fields were not completed: %s", data)
	}
}

func TestGrokResponsesUsesOfficialBuildSearchPair(t *testing.T) {
	var calls atomic.Int32
	var gotBody []byte
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp_ok","object":"response","status":"completed","model":"grok-4.5","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"No search needed"}]}],"usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer up.Close()

	s := New(log.New(io.Discard, "", 0))
	s.SetRoutes([]config.Route{facadeRoute("grok-success", "responses", "grok-4.5", "sk-grok", up.URL+"/v1")})
	startPathTestServer(t, s)
	data, status := postFacade(t, s, "grok-success", []byte(`{"input":"hi","stream":false}`), "Bearer oauth")
	if status != http.StatusOK || calls.Load() != 1 {
		t.Fatalf("status=%d calls=%d body=%s", status, calls.Load(), data)
	}
	tools, _, hosted, function, xSearch := summarizeBody(gotBody)
	if tools != 2 || hosted != 1 || function != 0 || xSearch != 1 {
		t.Fatalf("Grok request was not canonicalized: %s", gotBody)
	}
}

func TestResponsesFacadeSynthesizesSSEWhenUpstreamReturnsJSON(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp_1","object":"response","status":"completed","model":"gpt-real","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"OK"}]}],"usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer up.Close()

	s := New(log.New(io.Discard, "", 0))
	s.SetRoutes([]config.Route{facadeRoute("gpt-json", "responses", "gpt-real", "sk-channel", up.URL+"/v1")})
	startPathTestServer(t, s)
	body := []byte(`{"model":"gpt-real","input":"hi","stream":true}`)
	data, status := postFacade(t, s, "gpt-json", body, "Bearer oauth-super-grok")
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, data)
	}
	for _, expected := range [][]byte{
		[]byte("event: response.created"),
		[]byte("event: response.output_text.delta"),
		[]byte(`"annotations":[]`),
		[]byte("event: response.completed"),
	} {
		if !bytes.Contains(data, expected) {
			t.Fatalf("missing %q in synthesized SSE: %s", expected, data)
		}
	}
}

func TestResponsesFacadeSynthesizesEveryContentPart(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp_parts","object":"response","status":"completed","model":"gpt-real","output":[{"type":"reasoning","id":"rs_1","status":"completed","summary":[],"content":[{"type":"reasoning_text","text":"first thought"},{"type":"reasoning_text","text":"second thought"}]},{"type":"message","id":"msg_1","status":"completed","role":"assistant","content":[{"type":"output_text","text":"first answer","annotations":[],"logprobs":[]},{"type":"output_text","text":"second answer","annotations":[],"logprobs":[]},{"type":"refusal","refusal":"blocked detail"}]}],"usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer up.Close()

	s := New(log.New(io.Discard, "", 0))
	s.SetRoutes([]config.Route{facadeRoute("gpt-parts", "responses", "gpt-real", "sk-channel", up.URL+"/v1")})
	startPathTestServer(t, s)
	data, status := postFacade(t, s, "gpt-parts", []byte(`{"input":"hi","stream":true}`), "")
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, data)
	}
	for _, expected := range [][]byte{
		[]byte(`"content_index":0,"delta":"first thought"`),
		[]byte(`"content_index":1,"delta":"second thought"`),
		[]byte(`"content_index":0,"delta":"first answer"`),
		[]byte(`"content_index":1,"delta":"second answer"`),
		[]byte("event: response.refusal.delta"),
		[]byte(`"content_index":2,"delta":"blocked detail"`),
		[]byte("event: response.refusal.done"),
		[]byte("event: response.completed"),
	} {
		if !bytes.Contains(data, expected) {
			t.Fatalf("missing %q in synthesized SSE: %s", expected, data)
		}
	}
	if got := bytes.Count(data, []byte("event: response.content_part.done")); got != 5 {
		t.Fatalf("content_part.done count=%d, want 5: %s", got, data)
	}
}

func TestResponsesFacadeRejectsSSEForNonStreamingRequest(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"type":"response.completed","response":{"id":"resp_1","object":"response","status":"completed","model":"m","output":[]}}`+"\n\n")
	}))
	defer up.Close()

	s := New(log.New(io.Discard, "", 0))
	s.SetRoutes([]config.Route{facadeRoute("unexpected-sse", "responses", "m", "key", up.URL)})
	startPathTestServer(t, s)
	data, status := postFacade(t, s, "unexpected-sse", []byte(`{"input":"hi","stream":false}`), "")
	if status != http.StatusBadGateway || !bytes.Contains(data, []byte("ignored stream=false")) {
		t.Fatalf("status=%d body=%s", status, data)
	}
	if bytes.Contains(data, []byte("response.completed")) {
		t.Fatalf("SSE body leaked to non-streaming caller: %s", data)
	}
}

func TestMessagesFacadeProducesNativeResponsesWebSearchEvents(t *testing.T) {
	var gotBody []byte
	var gotKey, gotVersion string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		gotKey, gotVersion = r.Header.Get("X-Api-Key"), r.Header.Get("Anthropic-Version")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"thinking","thinking":"searching","signature":"sig"},{"type":"server_tool_use","id":"srv_1","name":"web_search","input":{"query":"q"}},{"type":"web_search_tool_result","tool_use_id":"srv_1","content":[{"type":"web_search_result","title":"Example","url":"https://example.test"}]},{"type":"text","text":"Example Domain"}],"model":"deepseek-v4-pro","stop_reason":"end_turn","usage":{"input_tokens":5,"cache_read_input_tokens":2,"output_tokens":3,"server_tool_use":{"web_search_requests":1}}}`)
	}))
	defer up.Close()

	s := New(log.New(io.Discard, "", 0))
	s.SetRoutes([]config.Route{facadeRoute("deepseek-pro", "messages", "deepseek-v4-pro", "sk-deepseek", up.URL+"/anthropic")})
	startPathTestServer(t, s)
	body := []byte(`{"model":"grok-4.5","input":[{"type":"message","role":"user","content":"search"}],"tools":[{"type":"web_search"},{"type":"x_search"}],"stream":true,"max_output_tokens":256}`)
	data, status := postFacade(t, s, "deepseek-pro", body, "Bearer oauth-super-grok")
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, data)
	}
	if gotKey != "sk-deepseek" || gotVersion != "2023-06-01" {
		t.Fatalf("messages auth headers key=%q version=%q", gotKey, gotVersion)
	}
	if bytes.Count(gotBody, []byte(`"name":"web_search"`)) != 1 || !bytes.Contains(gotBody, []byte(`"type":"web_search_20250305"`)) {
		t.Fatalf("hosted tool missing or duplicated: %s", gotBody)
	}
	if extractModel(gotBody) != "deepseek-v4-pro" || !bytes.Contains(gotBody, []byte(`"stream":false`)) {
		t.Fatalf("Messages request not converted: %s", gotBody)
	}
	for _, expected := range [][]byte{
		[]byte(`response.web_search_call.in_progress`),
		[]byte(`"type":"web_search_call"`),
		[]byte(`"url":"https://example.test"`),
		[]byte(`Example Domain`),
		[]byte(`response.completed`),
	} {
		if !bytes.Contains(data, expected) {
			t.Fatalf("missing %s in canonical SSE: %s", expected, data)
		}
	}
	if bytes.Contains(data, []byte(`server_tool_use`)) || bytes.Contains(data, []byte(`web_search_tool_result`)) {
		t.Fatalf("Anthropic-only block leaked to Build: %s", data)
	}
}

func TestMessagesFacadeReplaysNativeSearchBlocksOnNextTurn(t *testing.T) {
	var calls atomic.Int32
	var secondBody []byte
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		call := calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if call == 1 {
			_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"server_tool_use","id":"srv_1","name":"web_search","input":{"query":"q"}},{"type":"web_search_tool_result","tool_use_id":"srv_1","content":[{"type":"web_search_result","title":"Example","url":"https://example.test","page_age":"today","encrypted_content":"opaque-provider-state"}]},{"type":"text","text":"first answer"}],"model":"deepseek-v4-pro","stop_reason":"end_turn","usage":{"input_tokens":5,"output_tokens":3}}`)
			return
		}
		secondBody = body
		_, _ = io.WriteString(w, `{"id":"msg_2","type":"message","role":"assistant","content":[{"type":"text","text":"second answer"}],"model":"deepseek-v4-pro","stop_reason":"end_turn","usage":{"input_tokens":8,"output_tokens":2}}`)
	}))
	defer up.Close()

	s := New(log.New(io.Discard, "", 0))
	s.SetRoutes([]config.Route{facadeRoute("deepseek-pro", "messages", "deepseek-v4-pro", "sk-deepseek", up.URL+"/anthropic")})
	startPathTestServer(t, s)
	first := []byte(`{"model":"grok-4.5","input":[{"type":"message","role":"user","content":"search"}],"tools":[{"type":"web_search"}],"stream":false}`)
	if data, status := postFacade(t, s, "deepseek-pro", first, "Bearer oauth"); status != http.StatusOK {
		t.Fatalf("first status=%d body=%s", status, data)
	}
	second := []byte(`{
		"model":"grok-4.5",
		"input":[
			{"type":"message","role":"user","content":"search"},
			{"type":"web_search_call","id":"srv_1","status":"completed","action":{"type":"search","query":"q","sources":[{"type":"url","url":"https://example.test"}]}},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"first answer","annotations":[]}]},
			{"type":"message","role":"user","content":"follow up"}
		],
		"tools":[{"type":"web_search"}],
		"stream":false
	}`)
	if data, status := postFacade(t, s, "deepseek-pro", second, "Bearer oauth"); status != http.StatusOK {
		t.Fatalf("second status=%d body=%s", status, data)
	}
	if calls.Load() != 2 {
		t.Fatalf("upstream calls=%d", calls.Load())
	}
	for _, want := range [][]byte{
		[]byte(`"type":"server_tool_use"`),
		[]byte(`"type":"web_search_tool_result"`),
		[]byte(`"page_age":"today"`),
		[]byte(`"encrypted_content":"opaque-provider-state"`),
	} {
		if !bytes.Contains(secondBody, want) {
			t.Fatalf("second turn lost native replay field %s: %s", want, secondBody)
		}
	}
	if bytes.Contains(secondBody, []byte(`[backend web_search]`)) {
		t.Fatalf("Messages replay was degraded to synthetic text: %s", secondBody)
	}
}

func TestChatFacadeProducesCanonicalSearchAndFunctionCalls(t *testing.T) {
	var gotBody []byte
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chat_1","object":"chat.completion","created":1,"model":"grok-real","choices":[{"index":0,"message":{"role":"assistant","reasoning_content":"checked","content":"OK","tool_calls":[{"id":"call_1","type":"function","function":{"name":"save","arguments":"{\"ok\":true}"}}],"citations":["https://example.test"]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":4,"completion_tokens":2,"total_tokens":6}}`)
	}))
	defer up.Close()

	s := New(log.New(io.Discard, "", 0))
	s.SetRoutes([]config.Route{facadeRoute("grok-chat", "chat_completions", "grok-real", "sk-chat", up.URL+"/v1")})
	startPathTestServer(t, s)
	body := []byte(`{"model":"auxiliary-model","input":[{"role":"user","content":"search now"}],"tools":[{"type":"web_search"},{"type":"function","name":"save","parameters":{"type":"object"}}],"stream":false}`)
	data, status := postFacade(t, s, "grok-chat", body, "Bearer oauth")
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, data)
	}
	if !bytes.Contains(gotBody, []byte(`"search_parameters":{"mode":"auto","sources":[{"type":"web"}]}`)) || bytes.Contains(gotBody, []byte(`web_search_options`)) {
		t.Fatalf("Grok Chat search extension wrong: %s", gotBody)
	}
	if extractModel(gotBody) != "grok-real" {
		t.Fatalf("auxiliary model not rewritten: %s", gotBody)
	}
	var response map[string]any
	if err := json.Unmarshal(data, &response); err != nil {
		t.Fatal(err)
	}
	output := anySlice(response["output"])
	var types []string
	for _, raw := range output {
		item, _ := raw.(map[string]any)
		types = append(types, stringValue(item["type"]))
	}
	joined := strings.Join(types, ",")
	for _, want := range []string{"reasoning", "web_search_call", "function_call", "message"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %s in output types %s: %s", want, joined, data)
		}
	}
}

func TestMessagesCanonicalizationPreservesContentBlockOrder(t *testing.T) {
	result, err := canonicalFromMessages([]byte(`{
		"content":[
			{"type":"text","text":"before"},
			{"type":"tool_use","id":"call_1","name":"save","input":{"ok":true}},
			{"type":"text","text":"after"}
		]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Output) != 3 {
		t.Fatalf("output=%d want 3: %#v", len(result.Output), result.Output)
	}
	first := anySlice(result.Output[0].(map[string]any)["content"])[0].(map[string]any)
	last := anySlice(result.Output[2].(map[string]any)["content"])[0].(map[string]any)
	if first["text"] != "before" || result.Output[1].(map[string]any)["type"] != "function_call" || last["text"] != "after" {
		t.Fatalf("Messages content blocks were reordered: %#v", result.Output)
	}
}

func TestClientSearchWireAliasRoundTripsEveryUpstreamProtocol(t *testing.T) {
	for _, backend := range []string{"responses", "messages", "chat_completions"} {
		t.Run(backend, func(t *testing.T) {
			var gotBody []byte
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotBody, _ = io.ReadAll(r.Body)
				w.Header().Set("Content-Type", "application/json")
				switch backend {
				case "responses":
					_, _ = io.WriteString(w, `{"id":"resp_1","object":"response","created_at":1,"status":"completed","model":"grok-real","output":[{"type":"function_call","id":"fc_1","call_id":"call_1","name":"hellogrok_web_search","arguments":"{\"query\":\"q\"}","status":"completed"}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`)
				case "messages":
					_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"hellogrok_web_search","input":{"query":"q"}}],"model":"grok-real","stop_reason":"tool_use","usage":{"input_tokens":1,"output_tokens":1}}`)
				case "chat_completions":
					_, _ = io.WriteString(w, `{"id":"chat_1","object":"chat.completion","created":1,"model":"grok-real","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"hellogrok_web_search","arguments":"{\"query\":\"q\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
				}
			}))
			defer up.Close()

			route := facadeRoute("grok-client-"+backend, backend, "grok-real", "sk-test", up.URL+"/v1")
			route.SupportsBackendSearch = false
			s := New(log.New(io.Discard, "", 0))
			s.SetRoutes([]config.Route{route})
			startPathTestServer(t, s)
			body := []byte(`{"model":"grok-real","input":[{"type":"message","role":"user","content":"Use web_search now."}],"tools":[{"type":"function","name":"web_search","parameters":{"type":"object","properties":{"query":{"type":"string"}}}}],"tool_choice":{"type":"function","name":"web_search"},"stream":false}`)
			data, status := postFacade(t, s, route.ChannelID, body, "Bearer login-oauth")
			if status != http.StatusOK {
				t.Fatalf("status=%d body=%s", status, data)
			}
			if !bytes.Contains(gotBody, []byte(clientWebSearchWireAliasBase)) ||
				bytes.Contains(gotBody, []byte(`"name":"web_search"`)) {
				t.Fatalf("client search was not isolated on the upstream wire: %s", gotBody)
			}
			var response map[string]any
			if err := json.Unmarshal(data, &response); err != nil {
				t.Fatal(err)
			}
			output := anySlice(response["output"])
			if len(output) != 1 {
				t.Fatalf("unexpected canonical output: %s", data)
			}
			call, _ := output[0].(map[string]any)
			if stringValue(call["type"]) != "function_call" || stringValue(call["name"]) != "web_search" ||
				strings.Contains(string(data), clientWebSearchWireAliasBase) {
				t.Fatalf("wire alias leaked back to Build: %s", data)
			}
		})
	}
}

func TestChatFacadeDoesNotInventSearchWithoutUpstreamEvidence(t *testing.T) {
	result, err := canonicalFromChat([]byte(`{
		"choices":[{"message":{"role":"assistant","content":"No search was needed."},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":4,"completion_tokens":2}
	}`), true, "ordinary question")
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range result.Output {
		item, _ := raw.(map[string]any)
		if stringValue(item["type"]) == "web_search_call" {
			t.Fatalf("search event was fabricated: %#v", result.Output)
		}
	}

	withUsage, err := canonicalFromChat([]byte(`{
		"choices":[{"message":{"role":"assistant","content":"Search completed."},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":4,"completion_tokens":2,"server_tool_use":{"web_search_requests":1}}
	}`), true, "search question")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, raw := range withUsage.Output {
		item, _ := raw.(map[string]any)
		found = found || stringValue(item["type"]) == "web_search_call"
	}
	if !found {
		t.Fatalf("explicit server search usage was not represented: %#v", withUsage.Output)
	}
}

func TestWebFetchFunctionSurvivesEveryUpstreamProtocol(t *testing.T) {
	for _, backend := range []string{"responses", "messages", "chat_completions"} {
		t.Run(backend, func(t *testing.T) {
			var gotBody []byte
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotBody, _ = io.ReadAll(r.Body)
				w.Header().Set("Content-Type", "application/json")
				switch backend {
				case "responses":
					_, _ = io.WriteString(w, `{"id":"resp_fetch","object":"response","status":"completed","model":"generic","output":[{"type":"function_call","id":"fc_1","call_id":"call_fetch","name":"web_fetch","arguments":"{\"url\":\"https://example.test/page\"}","status":"completed"}],"usage":{"input_tokens":1,"output_tokens":1}}`)
				case "messages":
					_, _ = io.WriteString(w, `{"id":"msg_fetch","type":"message","role":"assistant","content":[{"type":"tool_use","id":"call_fetch","name":"web_fetch","input":{"url":"https://example.test/page"}}],"model":"generic","stop_reason":"tool_use","usage":{"input_tokens":1,"output_tokens":1}}`)
				case "chat_completions":
					_, _ = io.WriteString(w, `{"id":"chat_fetch","object":"chat.completion","model":"generic","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_fetch","type":"function","function":{"name":"web_fetch","arguments":"{\"url\":\"https://example.test/page\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
				}
			}))
			defer up.Close()

			s := New(log.New(io.Discard, "", 0))
			s.SetRoutes([]config.Route{facadeRoute("fetch-"+backend, backend, "generic", "sk-fetch", up.URL+"/v1")})
			startPathTestServer(t, s)
			request := []byte(`{
				"model":"generic",
				"input":"Fetch the supplied page",
				"tools":[{"type":"function","name":"web_fetch","description":"Fetch URL","parameters":{"type":"object","properties":{"url":{"type":"string"}},"required":["url"]}}],
				"stream":false
			}`)
			data, status := postFacade(t, s, "fetch-"+backend, request, "Bearer oauth")
			if status != http.StatusOK {
				t.Fatalf("status=%d body=%s", status, data)
			}
			if !bytes.Contains(gotBody, []byte(`"name":"web_fetch"`)) {
				t.Fatalf("web_fetch definition was lost upstream: %s", gotBody)
			}
			if !bytes.Contains(data, []byte(`"type":"function_call"`)) || !bytes.Contains(data, []byte(`"name":"web_fetch"`)) {
				t.Fatalf("web_fetch call was not returned canonically: %s", data)
			}
		})
	}
}

func TestFacadeRejectsMissingChannelKeyWithoutForwardingOAuth(t *testing.T) {
	var hits atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
	}))
	defer up.Close()
	s := New(log.New(io.Discard, "", 0))
	route := facadeRoute("no-key", "responses", "model", "", up.URL)
	route.Host = "third-party.example"
	s.SetRoutes([]config.Route{route})
	startPathTestServer(t, s)
	data, status := postFacade(t, s, "no-key", []byte(`{"model":"model","input":"hi"}`), "Bearer oauth-secret")
	if status != http.StatusUnauthorized || hits.Load() != 0 {
		t.Fatalf("status=%d hits=%d body=%s", status, hits.Load(), data)
	}
}

func TestFacadeAuthProviderIsolationFromLoginOAuth(t *testing.T) {
	var mu sync.Mutex
	type authHeaders struct {
		authorization string
		xAPIKey       string
	}
	seen := map[string]authHeaders{}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		seen[extractModel(body)] = authHeaders{authorization: r.Header.Get("Authorization"), xAPIKey: r.Header.Get("X-Api-Key")}
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp","object":"response","status":"completed","model":"x","output":[],"usage":{"input_tokens":0,"output_tokens":0}}`)
	}))
	defer up.Close()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	raw := fmt.Sprintf(`[auth_provider.channel]
command = "credential-helper"

[model.dynamic-grok]
model = "grok-custom"
base_url = %q
api_backend = "responses"
auth_provider = "channel"

[model.invalid-grok]
model = "grok-invalid"
base_url = %q
api_backend = "responses"
auth_provider = "undefined"

[model.dynamic-xapi]
model = "grok-dynamic-xapi"
base_url = %q
api_backend = "responses"
auth_scheme = "x_api_key"
auth_provider = "channel"

[model.static-grok]
model = "grok-static"
base_url = %q
api_backend = "responses"
api_key = "static-channel-key"
auth_provider = "channel"
`, up.URL, up.URL, up.URL, up.URL)
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	models, err := config.LoadModels(path)
	if err != nil {
		t.Fatal(err)
	}
	routes, err := config.BuildRoutes(models)
	if err != nil {
		t.Fatal(err)
	}
	for index := range routes {
		// The httptest origin is loopback, but these fixtures model remote custom
		// channels where missing credentials must fail closed.
		routes[index].Host = "custom-grok.example"
	}
	s := New(log.New(io.Discard, "", 0))
	s.SetRoutes(routes)
	startPathTestServer(t, s)

	if data, status := postFacade(t, s, "dynamic-grok", []byte(`{"model":"grok-custom","input":"hi"}`), "Bearer provider-token"); status != http.StatusOK {
		t.Fatalf("dynamic provider status=%d body=%s", status, data)
	}
	if data, status := postFacadeWithHeaders(t, s, "dynamic-xapi", []byte(`{"model":"grok-dynamic-xapi","input":"hi"}`), http.Header{"X-Api-Key": []string{"provider-xapi-token"}}); status != http.StatusOK {
		t.Fatalf("dynamic x-api-key provider status=%d body=%s", status, data)
	}
	if data, status := postFacade(t, s, "dynamic-xapi", []byte(`{"model":"grok-dynamic-xapi","input":"hi"}`), "Bearer login-oauth"); status != http.StatusUnauthorized {
		t.Fatalf("dynamic x-api-key route accepted bearer OAuth: status=%d body=%s", status, data)
	}
	if data, status := postFacade(t, s, "invalid-grok", []byte(`{"model":"grok-invalid","input":"hi"}`), "Bearer login-oauth"); status != http.StatusUnauthorized {
		t.Fatalf("undefined provider accepted login OAuth: status=%d body=%s", status, data)
	}
	if data, status := postFacade(t, s, "static-grok", []byte(`{"model":"grok-static","input":"hi"}`), "Bearer login-oauth"); status != http.StatusOK {
		t.Fatalf("static provider status=%d body=%s", status, data)
	}

	mu.Lock()
	defer mu.Unlock()
	if seen["grok-custom"].authorization != "Bearer provider-token" || seen["grok-custom"].xAPIKey != "" {
		t.Fatalf("valid provider token not forwarded: %#v", seen)
	}
	if seen["grok-dynamic-xapi"].xAPIKey != "provider-xapi-token" || seen["grok-dynamic-xapi"].authorization != "" {
		t.Fatalf("x-api-key provider token not isolated: %#v", seen)
	}
	if _, ok := seen["grok-invalid"]; ok {
		t.Fatalf("invalid channel reached upstream: %#v", seen)
	}
	if seen["grok-static"].authorization != "Bearer static-channel-key" || seen["grok-static"].xAPIKey != "" {
		t.Fatalf("login OAuth overrode static key: %#v", seen)
	}
}

func TestSameHostChannelsKeepDistinctKeysAndModels(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]string{}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		seen[extractModel(body)] = r.Header.Get("Authorization")
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp","object":"response","status":"completed","model":"x","output":[],"usage":{"input_tokens":0,"output_tokens":0}}`)
	}))
	defer up.Close()
	s := New(log.New(io.Discard, "", 0))
	s.SetRoutes([]config.Route{
		facadeRoute("one", "responses", "model-one", "key-one", up.URL),
		facadeRoute("two", "responses", "model-two", "key-two", up.URL),
	})
	startPathTestServer(t, s)
	for _, id := range []string{"one", "two"} {
		_, status := postFacade(t, s, id, []byte(`{"model":"grok-4.5","input":"hi"}`), "Bearer oauth")
		if status != http.StatusOK {
			t.Fatalf("channel %s status=%d", id, status)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if seen["model-one"] != "Bearer key-one" || seen["model-two"] != "Bearer key-two" {
		t.Fatalf("route auth mixed: %v", seen)
	}
}

func TestLegacyHostOnlyPathIsClosed(t *testing.T) {
	s := New(log.New(io.Discard, "", 0))
	startPathTestServer(t, s)
	_, status := postRaw(t, "http://"+s.PathAddr+"/u/example.test/v1/responses", []byte(`{}`))
	if status != http.StatusNotFound {
		t.Fatalf("legacy route status=%d", status)
	}
}

func TestFacadeRejectsBrowserAndNonJSONRequests(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp","object":"response","status":"completed","model":"m","output":[],"usage":{"input_tokens":0,"output_tokens":0}}`)
	}))
	defer up.Close()
	s := New(log.New(io.Discard, "", 0))
	s.SetRoutes([]config.Route{facadeRoute("guarded", "responses", "m", "key", up.URL)})
	startPathTestServer(t, s)
	target := "http://" + s.PathAddr + "/c/guarded/responses"

	tests := []struct {
		name        string
		host        string
		origin      string
		contentType string
		want        int
	}{
		{name: "dns rebinding host", host: "attacker.example", contentType: "application/json", want: http.StatusMisdirectedRequest},
		{name: "browser origin", origin: "https://attacker.example", contentType: "application/json", want: http.StatusForbidden},
		{name: "simple browser content type", contentType: "text/plain", want: http.StatusUnsupportedMediaType},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodPost, target, strings.NewReader(`{"input":"hi"}`))
			if test.host != "" {
				req.Host = test.host
			}
			if test.origin != "" {
				req.Header.Set("Origin", test.origin)
			}
			req.Header.Set("Content-Type", test.contentType)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != test.want {
				t.Fatalf("status=%d want=%d", resp.StatusCode, test.want)
			}
		})
	}
}

func TestFacadeRejectsUpstreamRedirectWithoutFollowingIt(t *testing.T) {
	var redirectedHits atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirectedHits.Add(1)
	}))
	defer destination.Close()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL, http.StatusTemporaryRedirect)
	}))
	defer up.Close()
	s := New(log.New(io.Discard, "", 0))
	s.SetRoutes([]config.Route{facadeRoute("redirect", "responses", "m", "key", up.URL)})
	startPathTestServer(t, s)
	data, status := postFacade(t, s, "redirect", []byte(`{"input":"hi"}`), "Bearer ignored")
	if status != http.StatusBadGateway || redirectedHits.Load() != 0 {
		t.Fatalf("status=%d redirected_hits=%d body=%s", status, redirectedHits.Load(), data)
	}
}

func TestResponsesFacadePatchesMultilineSSEEvent(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.completed\n")
		_, _ = io.WriteString(w, `data: {"type":"response.completed",`+"\n")
		_, _ = io.WriteString(w, `data: "response":{"object":"response","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"OK"}]}],"usage":{"input_tokens":1,"output_tokens":1}}}`+"\n\n")
	}))
	defer up.Close()
	s := New(log.New(io.Discard, "", 0))
	s.SetRoutes([]config.Route{facadeRoute("multiline", "responses", "wire-model", "key", up.URL)})
	startPathTestServer(t, s)
	data, status := postFacade(t, s, "multiline", []byte(`{"input":"hi","stream":true}`), "Bearer ignored")
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, data)
	}
	var payloads []string
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "data:") {
			payloads = append(payloads, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	if len(payloads) != 1 {
		t.Fatalf("multiline event was not collapsed to one patched payload: %s", data)
	}
	var event map[string]any
	if err := json.Unmarshal([]byte(payloads[0]), &event); err != nil {
		t.Fatalf("patched event is invalid JSON: %v\n%s", err, data)
	}
	if event["sequence_number"] != float64(0) {
		t.Fatalf("sequence_number=%v", event["sequence_number"])
	}
	response, _ := event["response"].(map[string]any)
	if response["id"] == nil || response["model"] != "wire-model" {
		t.Fatalf("strict response fields were not filled: %#v", response)
	}
}

func TestFacadeDropsUpstreamCookies(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Add("Set-Cookie", "upstream_session=secret; HttpOnly")
		_, _ = io.WriteString(w, `{"id":"resp","object":"response","status":"completed","model":"m","output":[],"usage":{"input_tokens":0,"output_tokens":0}}`)
	}))
	defer up.Close()
	s := New(log.New(io.Discard, "", 0))
	s.SetRoutes([]config.Route{facadeRoute("cookies", "responses", "m", "key", up.URL)})
	startPathTestServer(t, s)
	req, _ := http.NewRequest(http.MethodPost, "http://"+s.PathAddr+"/c/cookies/responses", strings.NewReader(`{"input":"hi"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if got := resp.Header.Values("Set-Cookie"); len(got) != 0 {
		t.Fatalf("upstream cookies leaked to loopback client: %v", got)
	}
}

func TestFacadeTransparentlyDecodesGzipResponses(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			t.Errorf("proxy did not advertise gzip support: %q", r.Header.Get("Accept-Encoding"))
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		writer := gzip.NewWriter(w)
		_, _ = io.WriteString(writer, `{"id":"resp","object":"response","status":"completed","model":"m","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"OK"}]}],"usage":{"input_tokens":1,"output_tokens":1}}`)
		_ = writer.Close()
	}))
	defer up.Close()
	s := New(log.New(io.Discard, "", 0))
	s.SetRoutes([]config.Route{facadeRoute("gzip", "responses", "m", "key", up.URL)})
	startPathTestServer(t, s)
	data, status := postFacade(t, s, "gzip", []byte(`{"input":"hi"}`), "")
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, data)
	}
	var response map[string]any
	if err := json.Unmarshal(data, &response); err != nil {
		t.Fatalf("decoded proxy response is invalid JSON: %v", err)
	}
	output := anySlice(response["output"])
	part := anySlice(output[0].(map[string]any)["content"])[0].(map[string]any)
	if _, ok := part["annotations"].([]any); !ok {
		t.Fatalf("gzip response was not patched: %s", data)
	}
}

func TestFacadeRejectsUnknownUpstreamContentEncoding(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "custom")
		_, _ = io.WriteString(w, `{}`)
	}))
	defer up.Close()
	s := New(log.New(io.Discard, "", 0))
	s.SetRoutes([]config.Route{facadeRoute("encoding", "responses", "m", "key", up.URL)})
	startPathTestServer(t, s)
	data, status := postFacade(t, s, "encoding", []byte(`{"input":"hi"}`), "")
	if status != http.StatusBadGateway || !bytes.Contains(data, []byte("unsupported content encoding")) {
		t.Fatalf("status=%d body=%s", status, data)
	}
}

func TestFacadeRejectsDeclaredOversizeRequestBeforeReading(t *testing.T) {
	s := New(log.New(io.Discard, "", 0))
	s.SetRoutes([]config.Route{facadeRoute("oversize-declared", "responses", "m", "key", "https://upstream.example/v1")})
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/c/oversize-declared/responses", strings.NewReader(`{}`))
	request.Host = "127.0.0.1"
	request.Header.Set("Content-Type", "application/json")
	request.ContentLength = maxFacadeBodyBytes + 1
	recorder := httptest.NewRecorder()
	s.servePath(recorder, request)
	if recorder.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.Bytes())
	}
}

func TestFacadeBoundsMultiLineSSEEvent(t *testing.T) {
	var logs bytes.Buffer
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		chunk := strings.Repeat("x", 1<<20)
		for range 17 {
			_, _ = fmt.Fprintf(w, "data: %s\n", chunk)
		}
		_, _ = io.WriteString(w, "\n")
	}))
	defer up.Close()
	s := New(log.New(&logs, "", 0))
	s.SetRoutes([]config.Route{facadeRoute("large-sse", "responses", "m", "key", up.URL)})
	startPathTestServer(t, s)
	data, status := postFacade(t, s, "large-sse", []byte(`{"input":"hi","stream":true}`), "")
	if status != http.StatusOK {
		t.Fatalf("status=%d", status)
	}
	if !strings.Contains(logs.String(), "SSE event exceeds 16 MiB") {
		t.Fatalf("oversize SSE event was not rejected: %s", logs.String())
	}
	if !bytes.Contains(data, []byte("event: error")) || !bytes.Contains(data, []byte(`"code":"proxy_stream_error"`)) {
		t.Fatalf("oversize SSE failure was not surfaced to the client: %s", data[len(data)-min(len(data), 512):])
	}
}

func TestFacadeSurfacesPrematureSSEEnd(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `event: response.created`+"\n"+
			`data: {"type":"response.created","response":{"id":"resp_1","object":"response","status":"in_progress","model":"m","output":[]}}`+"\n\n")
	}))
	defer up.Close()
	s := New(log.New(io.Discard, "", 0))
	s.SetRoutes([]config.Route{facadeRoute("truncated-sse", "responses", "m", "key", up.URL)})
	startPathTestServer(t, s)
	data, status := postFacade(t, s, "truncated-sse", []byte(`{"input":"hi","stream":true}`), "")
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, data)
	}
	if !bytes.Contains(data, []byte("event: error")) ||
		!bytes.Contains(data, []byte("ended without a terminal event")) {
		t.Fatalf("premature SSE end was silent: %s", data)
	}
}

func TestFacadeRejectsMalformedNonStreamResponseBeforeStreaming(t *testing.T) {
	var requests atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if requests.Add(1) == 1 {
			_, _ = io.WriteString(w, `{"id":"resp_bad","object":"response","status":"completed","model":"m","output":[null],"usage":{"input_tokens":0,"output_tokens":0}}`)
			return
		}
		_, _ = io.WriteString(w, `{"id":"resp_ok","object":"response","status":"completed","model":"m","output":[],"usage":{"input_tokens":0,"output_tokens":0}}`)
	}))
	defer up.Close()
	s := New(log.New(io.Discard, "", 0))
	s.SetRoutes([]config.Route{facadeRoute("malformed-json-stream", "responses", "m", "key", up.URL)})
	startPathTestServer(t, s)

	data, status := postFacade(t, s, "malformed-json-stream", []byte(`{"input":"hi","stream":true}`), "")
	if status != http.StatusBadGateway || !bytes.Contains(data, []byte("output[0] must be an object")) {
		t.Fatalf("status=%d body=%s", status, data)
	}

	data, status = postFacade(t, s, "malformed-json-stream", []byte(`{"input":"hi","stream":true}`), "")
	if status != http.StatusOK || !bytes.Contains(data, []byte("response.completed")) {
		t.Fatalf("proxy did not recover after malformed response: status=%d body=%s", status, data)
	}
}

func TestFacadeRejectsMalformedIntermediateSSEFrame(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `event: response.created`+"\n"+
			`data: {"type":"response.created","response":{"id":"resp_1","object":"response","status":"in_progress","model":"m","output":[]}}`+"\n\n"+
			`event: response.output_text.delta`+"\n"+
			`data: {"type":"response.output_text.delta","delta":`+"\n\n"+
			`event: response.completed`+"\n"+
			`data: {"type":"response.completed","response":{"id":"resp_1","object":"response","status":"completed","model":"m","output":[]}}`+"\n\n")
	}))
	defer up.Close()
	s := New(log.New(io.Discard, "", 0))
	s.SetRoutes([]config.Route{facadeRoute("malformed-sse", "responses", "m", "key", up.URL)})
	startPathTestServer(t, s)
	data, status := postFacade(t, s, "malformed-sse", []byte(`{"input":"hi","stream":true}`), "")
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, data)
	}
	if !bytes.Contains(data, []byte("event: error")) ||
		!bytes.Contains(data, []byte("upstream Responses stream failed")) {
		t.Fatalf("malformed SSE frame was not surfaced: %s", data)
	}
	if bytes.Contains(data, []byte("response.completed")) {
		t.Fatalf("frames after malformed SSE data were forwarded: %s", data)
	}
}

func TestFacadeAssignsIncreasingMissingSSESequenceNumbers(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"type":"response.created","response":{"id":"resp_1","object":"response","status":"in_progress","model":"m","output":[]}}`+"\n\n"+
			`data: {"type":"response.completed","response":{"id":"resp_1","object":"response","status":"completed","model":"m","output":[]}}`+"\n\n")
	}))
	defer up.Close()
	s := New(log.New(io.Discard, "", 0))
	s.SetRoutes([]config.Route{facadeRoute("sequence-sse", "responses", "m", "key", up.URL)})
	startPathTestServer(t, s)
	data, status := postFacade(t, s, "sequence-sse", []byte(`{"input":"hi","stream":true}`), "")
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, data)
	}
	if !bytes.Contains(data, []byte(`"sequence_number":0`)) || !bytes.Contains(data, []byte(`"sequence_number":1`)) {
		t.Fatalf("missing SSE sequence numbers were not assigned monotonically: %s", data)
	}
}

func postRaw(t *testing.T, target string, body []byte) ([]byte, int) {
	t.Helper()
	resp, err := http.Post(target, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return data, resp.StatusCode
}

func postFacadeWithHeaders(t *testing.T, s *Server, channel string, body []byte, headers http.Header) ([]byte, int) {
	t.Helper()
	target := "http://" + s.PathAddr + "/c/" + url.PathEscape(channel) + "/responses"
	req, err := http.NewRequest(http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	for name, values := range headers {
		for _, value := range values {
			req.Header.Add(name, value)
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return data, resp.StatusCode
}
