package proxy

import (
	"bufio"
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
	return config.Route{
		ChannelID:             id,
		Host:                  host,
		OriginBase:            origin,
		APIBackend:            backend,
		WireModel:             model,
		APIKey:                key,
		SupportsBackendSearch: true,
	}
}

func xAPIKeyFacadeRoute(id, backend, model, key, origin string) config.Route {
	route := facadeRoute(id, backend, model, key, origin)
	route.AuthScheme = "x_api_key"
	return route
}

func postFacade(t *testing.T, s *Server, channel string, body []byte, auth string) ([]byte, int) {
	data, status, _ := postFacadeResponse(t, s, channel, body, auth)
	return data, status
}

func postFacadeResponse(t *testing.T, s *Server, channel string, body []byte, auth string) ([]byte, int, http.Header) {
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
	return data, resp.StatusCode, resp.Header.Clone()
}

func buildClientSearchRequest() []byte {
	return []byte(`{"model":"grok-4.5","input":"search","max_output_tokens":8192,"store":false,"stream":false,"temperature":0.1,"top_p":0.95,"tools":[{"type":"web_search"}]}`)
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

func TestResponsesFacadeRoutesDottedAndDashedChannelIDWithChannelAuth(t *testing.T) {
	var gotAuth string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp_special","object":"response","status":"completed","model":"wire.model-v1","output":[],"usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer up.Close()

	const channelID = "provider.v1-beta"
	s := New(log.New(io.Discard, "", 0))
	s.SetRoutes([]config.Route{facadeRoute(channelID, "responses", "wire.model-v1", "special-key", up.URL+"/v1")})
	startPathTestServer(t, s)
	data, status := postFacade(t, s, channelID, []byte(`{"input":"hi","stream":false}`), "Bearer grok-session-token")
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, data)
	}
	if gotAuth != "Bearer special-key" {
		t.Fatalf("upstream authorization = %q", gotAuth)
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

func TestFacadeLogsBufferedFallbackWhenUpstreamIgnoresStreaming(t *testing.T) {
	tests := []struct {
		backend  string
		response string
	}{
		{
			backend:  "responses",
			response: `{"id":"resp_1","object":"response","status":"completed","model":"model","output":[{"type":"message","id":"msg_1","status":"completed","role":"assistant","content":[{"type":"output_text","text":"answer","annotations":[],"logprobs":[]}]}],"usage":{"input_tokens":1,"output_tokens":1}}`,
		},
		{
			backend:  "messages",
			response: `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"answer"}],"model":"model","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`,
		},
		{
			backend:  "chat_completions",
			response: `{"id":"chat_1","object":"chat.completion","model":"model","choices":[{"index":0,"message":{"role":"assistant","content":"answer"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`,
		},
	}
	for _, test := range tests {
		t.Run(test.backend, func(t *testing.T) {
			var gotBody []byte
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotBody, _ = io.ReadAll(r.Body)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, test.response)
			}))
			defer up.Close()

			var logs bytes.Buffer
			s := New(log.New(&logs, "", 0))
			s.SetRoutes([]config.Route{facadeRoute("json-fallback", test.backend, "model", "key", up.URL)})
			startPathTestServer(t, s)
			data, status := postFacade(t, s, "json-fallback", []byte(`{"input":"hi","stream":true}`), "")
			if status != http.StatusOK {
				t.Fatalf("status=%d body=%s", status, data)
			}
			if !bytes.Contains(gotBody, []byte(`"stream":true`)) {
				t.Fatalf("upstream streaming was not requested: %s", gotBody)
			}
			if !bytes.Contains(data, []byte(`response.output_text.delta`)) || !bytes.Contains(data, []byte(`response.completed`)) {
				t.Fatalf("buffered fallback was not a valid Responses stream: %s", data)
			}
			if !strings.Contains(logs.String(), "ignored stream=true; emitting buffered JSON fallback") {
				t.Fatalf("buffered fallback was not disclosed in logs: %s", logs.String())
			}
		})
	}
}

func TestResponsesFacadePreservesNativeSearchSources(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w,
			`data: {"type":"response.web_search_call.in_progress","sequence_number":0,"output_index":0,"item_id":"ws_1"}`+"\n\n"+
				`data: {"type":"response.output_item.done","sequence_number":1,"output_index":0,"item":{"type":"web_search_call","id":"ws_1","status":"completed","action":{"type":"search","query":"query","sources":[{"type":"url","url":"https://one.example.test/a"},{"type":"url","url":"https://two.example.test/b"}]}}}`+"\n\n"+
				`data: {"type":"response.completed","sequence_number":2,"response":{"id":"resp_1","object":"response","status":"completed","model":"model","output":[{"type":"web_search_call","id":"ws_1","status":"completed","action":{"type":"search","query":"query","sources":[{"type":"url","url":"https://one.example.test/a"},{"type":"url","url":"https://two.example.test/b"}]}}],"usage":{"input_tokens":1,"output_tokens":1}}}`+"\n\n")
	}))
	defer up.Close()

	s := New(log.New(io.Discard, "", 0))
	s.SetRoutes([]config.Route{facadeRoute("native-sources", "responses", "model", "key", up.URL)})
	startPathTestServer(t, s)
	data, status := postFacade(t, s, "native-sources", []byte(`{"input":"search","tools":[{"type":"web_search"}],"stream":true}`), "")
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, data)
	}
	for _, source := range [][]byte{[]byte(`"url":"https://one.example.test/a"`), []byte(`"url":"https://two.example.test/b"`)} {
		if !bytes.Contains(data, source) {
			t.Fatalf("native Responses search source was lost: %s", data)
		}
	}
}

func TestClientSearchAllProtocolsBackfillVerifiedAnswerURLs(t *testing.T) {
	const sourceURL = "https://docs.example.test/search-result"
	tests := []struct {
		backend     string
		response    string
		requestMark string
	}{
		{
			backend:     "responses",
			requestMark: `"type":"web_search"`,
			response: `{"id":"resp_1","object":"response","status":"completed","model":"model","output":[` +
				`{"type":"web_search_call","id":"ws_1","status":"completed","action":{"type":"search","query":"query","sources":[]}},` +
				`{"type":"message","id":"msg_1","status":"completed","role":"assistant","content":[{"type":"output_text","text":"See [Docs](` + sourceURL + `).","annotations":[],"logprobs":[]}]}],` +
				`"usage":{"input_tokens":1,"output_tokens":1}}`,
		},
		{
			backend:     "messages",
			requestMark: `"type":"web_search_20250305"`,
			response: `{"id":"msg_1","type":"message","role":"assistant","model":"model","content":[` +
				`{"type":"server_tool_use","id":"ws_1","name":"web_search","input":{"query":"query"}},` +
				`{"type":"web_search_tool_result","tool_use_id":"ws_1","content":[]},` +
				`{"type":"text","text":"See [Docs](` + sourceURL + `)."}],` +
				`"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`,
		},
		{
			backend:     "chat_completions",
			requestMark: `"web_search_options":{}`,
			response: `{"id":"chat_1","object":"chat.completion","model":"model","choices":[{"index":0,"message":{"role":"assistant","content":"See [Docs](` + sourceURL + `)."},"finish_reason":"stop"}],` +
				`"usage":{"prompt_tokens":1,"completion_tokens":1,"web_search_requests":1}}`,
		},
	}
	for _, test := range tests {
		t.Run(test.backend, func(t *testing.T) {
			var upstreamRequest []byte
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				upstreamRequest, _ = io.ReadAll(r.Body)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, test.response)
			}))
			defer up.Close()

			s := New(log.New(io.Discard, "", 0))
			route := facadeRoute("source-backfill", test.backend, "model", "key", up.URL)
			route.SupportsBackendSearch = false
			s.SetRoutes([]config.Route{route})
			startPathTestServer(t, s)
			data, status := postFacade(t, s, "source-backfill", buildClientSearchRequest(), "")
			if status != http.StatusOK {
				t.Fatalf("status=%d body=%s", status, data)
			}
			if !bytes.Contains(upstreamRequest, []byte(test.requestMark)) {
				t.Fatalf("Build client search was not converted to native %s search: %s", test.backend, upstreamRequest)
			}
			if !bytes.Contains(data, []byte(`"type":"web_search_call"`)) || !bytes.Contains(data, []byte(`"url":"`+sourceURL+`"`)) {
				t.Fatalf("search source was not backfilled: %s", data)
			}
			if !responseHasURLCitation(t, data, sourceURL) {
				t.Fatalf("search source was not mirrored to output_text annotations: %s", data)
			}
		})
	}
}

func TestClientSearchAllProtocolsMirrorStructuredSources(t *testing.T) {
	const sourceOne = "https://source-one.example.test/article"
	const sourceTwo = "https://source-two.example.test/report"
	tests := []struct {
		backend  string
		response string
	}{
		{
			backend: "responses",
			response: `{"id":"resp_1","object":"response","status":"completed","model":"model","output":[` +
				`{"type":"web_search_call","id":"ws_1","status":"completed","action":{"type":"search","query":"query","sources":[{"type":"url","url":"` + sourceOne + `"},{"type":"url","url":"` + sourceTwo + `"}]}},` +
				`{"type":"message","id":"msg_1","status":"completed","role":"assistant","content":[{"type":"output_text","text":"answer","annotations":[],"logprobs":[]}]}],` +
				`"usage":{"input_tokens":1,"output_tokens":1}}`,
		},
		{
			backend: "messages",
			response: `{"id":"msg_1","type":"message","role":"assistant","model":"model","content":[` +
				`{"type":"server_tool_use","id":"ws_1","name":"web_search","input":{"query":"query"}},` +
				`{"type":"web_search_tool_result","tool_use_id":"ws_1","content":[` +
				`{"type":"web_search_result","title":"One","url":"` + sourceOne + `"},` +
				`{"type":"web_search_result","title":"Two","url":"` + sourceTwo + `"}]},` +
				`{"type":"text","text":"answer"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`,
		},
		{
			backend: "chat_completions",
			response: `{"id":"chat_1","object":"chat.completion","model":"model","citations":[` +
				`{"title":"One","url":"` + sourceOne + `"},{"title":"Two","url":"` + sourceTwo + `"}],` +
				`"choices":[{"index":0,"message":{"role":"assistant","content":"answer"},"finish_reason":"stop"}],` +
				`"usage":{"prompt_tokens":1,"completion_tokens":1}}`,
		},
	}
	for _, test := range tests {
		t.Run(test.backend, func(t *testing.T) {
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, test.response)
			}))
			defer up.Close()

			s := New(log.New(io.Discard, "", 0))
			route := facadeRoute("structured-sources", test.backend, "model", "key", up.URL)
			route.SupportsBackendSearch = false
			s.SetRoutes([]config.Route{route})
			startPathTestServer(t, s)
			data, status := postFacade(t, s, "structured-sources", buildClientSearchRequest(), "")
			if status != http.StatusOK {
				t.Fatalf("status=%d body=%s", status, data)
			}
			found, sources := responseSearchSources(t, data)
			if !found || len(sources) != 2 {
				t.Fatalf("structured %s sources were not exposed through action.sources: %v %s", test.backend, sources, data)
			}
			for _, sourceURL := range []string{sourceOne, sourceTwo} {
				if !responseHasURLCitation(t, data, sourceURL) {
					t.Fatalf("structured %s source was not exposed to Build client citations: %s", test.backend, data)
				}
			}
		})
	}
}

func TestClientSearchAllProtocolsKeepEmptySourcesHonest(t *testing.T) {
	tests := []struct {
		backend  string
		response string
	}{
		{
			backend: "responses",
			response: `{"id":"resp_1","object":"response","status":"completed","model":"model","output":[` +
				`{"type":"web_search_call","id":"ws_1","status":"completed","action":{"type":"search","query":"query","sources":[]}},` +
				`{"type":"message","id":"msg_1","status":"completed","role":"assistant","content":[{"type":"output_text","text":"answer without URLs","annotations":[],"logprobs":[]}]}],` +
				`"usage":{"input_tokens":1,"output_tokens":1}}`,
		},
		{
			backend: "messages",
			response: `{"id":"msg_1","type":"message","role":"assistant","model":"model","content":[` +
				`{"type":"server_tool_use","id":"ws_1","name":"web_search","input":{"query":"query"}},` +
				`{"type":"web_search_tool_result","tool_use_id":"ws_1","content":[]},` +
				`{"type":"text","text":"answer without URLs"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`,
		},
		{
			backend: "chat_completions",
			response: `{"id":"chat_1","object":"chat.completion","model":"model","choices":[{"index":0,"message":{"role":"assistant","content":"answer without URLs"},"finish_reason":"stop"}],` +
				`"usage":{"prompt_tokens":1,"completion_tokens":1,"web_search_requests":1}}`,
		},
	}
	for _, test := range tests {
		t.Run(test.backend, func(t *testing.T) {
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, test.response)
			}))
			defer up.Close()

			s := New(log.New(io.Discard, "", 0))
			route := facadeRoute("empty-search-sources", test.backend, "model", "key", up.URL)
			route.SupportsBackendSearch = false
			s.SetRoutes([]config.Route{route})
			startPathTestServer(t, s)
			data, status := postFacade(t, s, "empty-search-sources", buildClientSearchRequest(), "")
			if status != http.StatusOK {
				t.Fatalf("status=%d body=%s", status, data)
			}
			found, sources := responseSearchSources(t, data)
			if !found || len(sources) != 0 {
				t.Fatalf("search without real URLs must have an empty source list: %v %s", sources, data)
			}
			if bytes.Contains(data, []byte(`"type":"url_citation"`)) {
				t.Fatalf("search without real URLs gained a fabricated citation: %s", data)
			}
		})
	}
}

func TestClientSearchAllProtocolsDoNotInventSearchFromAnswerLinks(t *testing.T) {
	tests := []struct {
		backend  string
		response string
	}{
		{
			backend:  "responses",
			response: `{"id":"resp_1","object":"response","status":"completed","model":"model","output":[{"type":"message","id":"msg_1","status":"completed","role":"assistant","content":[{"type":"output_text","text":"Project: https://example.test/docs","annotations":[],"logprobs":[]}]}],"usage":{"input_tokens":1,"output_tokens":1}}`,
		},
		{
			backend:  "messages",
			response: `{"id":"msg_1","type":"message","role":"assistant","model":"model","content":[{"type":"text","text":"Project: https://example.test/docs"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`,
		},
		{
			backend:  "chat_completions",
			response: `{"id":"chat_1","object":"chat.completion","model":"model","choices":[{"index":0,"message":{"role":"assistant","content":"Project: https://example.test/docs"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`,
		},
	}
	for _, test := range tests {
		t.Run(test.backend, func(t *testing.T) {
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, test.response)
			}))
			defer up.Close()

			s := New(log.New(io.Discard, "", 0))
			route := facadeRoute("no-fake-search", test.backend, "model", "key", up.URL)
			route.SupportsBackendSearch = false
			s.SetRoutes([]config.Route{route})
			startPathTestServer(t, s)
			data, status := postFacade(t, s, "no-fake-search", buildClientSearchRequest(), "")
			if status != http.StatusOK {
				t.Fatalf("status=%d body=%s", status, data)
			}
			if bytes.Contains(data, []byte(`"type":"web_search_call"`)) {
				t.Fatalf("ordinary %s answer link was misreported as a search: %s", test.backend, data)
			}
			if bytes.Contains(data, []byte(`"type":"url_citation"`)) {
				t.Fatalf("ordinary %s answer link was misreported as a search citation: %s", test.backend, data)
			}
		})
	}
}

func TestStreamingFacadesBackfillSearchSources(t *testing.T) {
	const sourceURL = "https://news.example.test/item"
	tests := []struct {
		backend string
		stream  string
	}{
		{
			backend: "responses",
			stream: `data: {"type":"response.web_search_call.in_progress","sequence_number":0,"output_index":0,"item_id":"ws_1"}` + "\n\n" +
				`data: {"type":"response.output_item.done","sequence_number":1,"output_index":0,"item":{"type":"web_search_call","id":"ws_1","status":"completed","action":{"type":"search","query":"query","sources":[]}}}` + "\n\n" +
				`data: {"type":"response.output_text.delta","sequence_number":2,"output_index":1,"item_id":"msg_1","content_index":0,"delta":"See ` + sourceURL + `"}` + "\n\n" +
				`data: {"type":"response.output_text.done","sequence_number":3,"output_index":1,"item_id":"msg_1","content_index":0,"text":"See ` + sourceURL + `"}` + "\n\n" +
				`data: {"type":"response.completed","sequence_number":4,"response":{"id":"resp_1","object":"response","status":"completed","model":"model","output":[{"type":"web_search_call","id":"ws_1","status":"completed","action":{"type":"search","query":"query","sources":[]}},{"type":"message","id":"msg_1","status":"completed","role":"assistant","content":[{"type":"output_text","text":"See ` + sourceURL + `","annotations":[],"logprobs":[]}]}],"usage":{"input_tokens":1,"output_tokens":1}}}` + "\n\n",
		},
		{
			backend: "messages",
			stream: `data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"model":"model","usage":{"input_tokens":1,"output_tokens":0}}}` + "\n\n" +
				`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
				`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"See ` + sourceURL + `"}}` + "\n\n" +
				`data: {"type":"content_block_stop","index":0}` + "\n\n" +
				`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1,"server_tool_use":{"web_search_requests":1}}}` + "\n\n" +
				`data: {"type":"message_stop"}` + "\n\n",
		},
		{
			backend: "chat_completions",
			stream: `data: {"id":"chat_1","object":"chat.completion.chunk","model":"model","choices":[{"index":0,"delta":{"role":"assistant","content":"See ` + sourceURL + `"},"finish_reason":null}],"usage":{"web_search_requests":1}}` + "\n\n" +
				`data: {"id":"chat_1","object":"chat.completion.chunk","model":"model","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"web_search_requests":1}}` + "\n\n" +
				"data: [DONE]\n\n",
		},
	}
	for _, test := range tests {
		t.Run(test.backend, func(t *testing.T) {
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, test.stream)
			}))
			defer up.Close()

			s := New(log.New(io.Discard, "", 0))
			s.SetRoutes([]config.Route{facadeRoute("stream-source-backfill", test.backend, "model", "key", up.URL)})
			startPathTestServer(t, s)
			data, status := postFacade(t, s, "stream-source-backfill", []byte(`{"input":"search","tools":[{"type":"web_search"}],"stream":true}`), "")
			if status != http.StatusOK {
				t.Fatalf("status=%d body=%s", status, data)
			}
			var found, foundCitation bool
			for _, frame := range strings.Split(string(data), "\n\n") {
				if strings.Contains(frame, `"type":"response.output_item.done"`) &&
					strings.Contains(frame, `"type":"web_search_call"`) && strings.Contains(frame, `"url":"`+sourceURL+`"`) {
					found = true
				}
				if strings.Contains(frame, `"type":"response.completed"`) &&
					strings.Contains(frame, `"type":"url_citation"`) && strings.Contains(frame, `"url":"`+sourceURL+`"`) {
					foundCitation = true
				}
			}
			if !found {
				t.Fatalf("streaming search completion did not contain the backfilled source: %s", data)
			}
			if !foundCitation {
				t.Fatalf("streaming terminal response did not contain the client citation: %s", data)
			}
			if !bytes.Contains(data, []byte(`response.output_text.delta`)) || !bytes.Contains(data, []byte(`response.completed`)) {
				t.Fatalf("streaming response lost text or terminal events: %s", data)
			}
		})
	}
}

func responseHasURLCitation(t *testing.T, data []byte, sourceURL string) bool {
	t.Helper()
	root, err := decodeJSONMap(data)
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range anySlice(root["output"]) {
		item, _ := raw.(map[string]any)
		if stringValue(item["type"]) != "message" {
			continue
		}
		for _, rawPart := range anySlice(item["content"]) {
			part, _ := rawPart.(map[string]any)
			for _, rawAnnotation := range anySlice(part["annotations"]) {
				annotation, _ := rawAnnotation.(map[string]any)
				if stringValue(annotation["type"]) == "url_citation" && stringValue(annotation["url"]) == sourceURL {
					return true
				}
			}
		}
	}
	return false
}

func responseSearchSources(t *testing.T, data []byte) (bool, []string) {
	t.Helper()
	root, err := decodeJSONMap(data)
	if err != nil {
		t.Fatal(err)
	}
	var urls []string
	found := false
	for _, raw := range anySlice(root["output"]) {
		item, _ := raw.(map[string]any)
		if stringValue(item["type"]) != "web_search_call" {
			continue
		}
		found = true
		action, _ := item["action"].(map[string]any)
		urls = mergeUniqueStrings(urls, urlsFromJSON(action["sources"])...)
	}
	return found, urls
}

func TestResponsesSearchSourceBackfillDoesNotBufferText(t *testing.T) {
	release := make(chan struct{})
	var releaseOnce sync.Once
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"type":"response.output_item.done","sequence_number":0,"output_index":0,"item":{"type":"web_search_call","id":"ws_1","status":"completed","action":{"type":"search","query":"query","sources":[]}}}`+"\n\n"+
			`data: {"type":"response.output_text.delta","sequence_number":1,"output_index":1,"item_id":"msg_1","content_index":0,"delta":"first https://news.example.test/item"}`+"\n\n")
		w.(http.Flusher).Flush()
		<-release
		_, _ = io.WriteString(w, `data: {"type":"response.completed","sequence_number":2,"response":{"id":"resp_1","object":"response","status":"completed","model":"model","output":[{"type":"web_search_call","id":"ws_1","status":"completed","action":{"type":"search","query":"query","sources":[]}},{"type":"message","id":"msg_1","status":"completed","role":"assistant","content":[{"type":"output_text","text":"first https://news.example.test/item","annotations":[],"logprobs":[]}]}],"usage":{"input_tokens":1,"output_tokens":1}}}`+"\n\n")
	}))
	defer up.Close()
	defer releaseOnce.Do(func() { close(release) })

	s := New(log.New(io.Discard, "", 0))
	s.SetRoutes([]config.Route{facadeRoute("search-stream-timing", "responses", "model", "key", up.URL)})
	startPathTestServer(t, s)
	req, _ := http.NewRequest(http.MethodPost, "http://"+s.PathAddr+"/c/search-stream-timing/responses", strings.NewReader(`{"input":"search","tools":[{"type":"web_search"}],"stream":true}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	reader := bufio.NewReader(resp.Body)
	seen := make(chan string, 1)
	go func() {
		var body strings.Builder
		for {
			line, readErr := reader.ReadString('\n')
			body.WriteString(line)
			if strings.Contains(body.String(), `"delta":"first https://news.example.test/item"`) || readErr != nil {
				seen <- body.String()
				return
			}
		}
	}()
	select {
	case body := <-seen:
		if !strings.Contains(body, `"delta":"first`) {
			t.Fatalf("stream ended before text delta: %s", body)
		}
	case <-time.After(time.Second):
		t.Fatal("text delta was buffered while waiting to backfill search sources")
	}
	releaseOnce.Do(func() { close(release) })
	_, _ = io.ReadAll(reader)
}

func TestFacadeStreamsFirstTextDeltaBeforeUpstreamCompletes(t *testing.T) {
	tests := []struct {
		backend string
		first   string
		rest    string
	}{
		{
			backend: "responses",
			first:   `data: {"type":"response.output_text.delta","sequence_number":0,"output_index":0,"item_id":"msg_1","content_index":0,"delta":"first"}` + "\n\n",
			rest:    `data: {"type":"response.completed","sequence_number":1,"response":{"id":"resp_1","object":"response","status":"completed","model":"model","output":[],"usage":{"input_tokens":1,"output_tokens":1}}}` + "\n\n",
		},
		{
			backend: "messages",
			first: `data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"model":"model","usage":{"input_tokens":1,"output_tokens":0}}}` + "\n\n" +
				`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
				`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"first"}}` + "\n\n",
			rest: `data: {"type":"content_block_stop","index":0}` + "\n\n" +
				`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}` + "\n\n" +
				`data: {"type":"message_stop"}` + "\n\n",
		},
		{
			backend: "chat_completions",
			first:   `data: {"id":"chat_1","object":"chat.completion.chunk","model":"model","choices":[{"index":0,"delta":{"role":"assistant","content":"first"},"finish_reason":null}]}` + "\n\n",
			rest: `data: {"id":"chat_1","object":"chat.completion.chunk","model":"model","choices":[{"index":0,"delta":{"content":" second"},"finish_reason":null}]}` + "\n\n" +
				`data: {"id":"chat_1","object":"chat.completion.chunk","model":"model","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":2}}` + "\n\n" +
				"data: [DONE]\n\n",
		},
	}

	for _, test := range tests {
		t.Run(test.backend, func(t *testing.T) {
			release := make(chan struct{})
			var releaseOnce sync.Once
			defer releaseOnce.Do(func() { close(release) })
			var upstreamStream atomic.Bool
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				upstreamStream.Store(bytes.Contains(body, []byte(`"stream":true`)))
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, test.first)
				w.(http.Flusher).Flush()
				<-release
				_, _ = io.WriteString(w, test.rest)
			}))
			defer up.Close()

			s := New(log.New(io.Discard, "", 0))
			s.SetRoutes([]config.Route{facadeRoute("stream-timing", test.backend, "model", "key", up.URL)})
			startPathTestServer(t, s)
			req, _ := http.NewRequest(http.MethodPost, "http://"+s.PathAddr+"/c/stream-timing/responses", strings.NewReader(`{"input":"hi","stream":true}`))
			req.Header.Set("Content-Type", "application/json")
			resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				data, _ := io.ReadAll(resp.Body)
				t.Fatalf("status=%d body=%s", resp.StatusCode, data)
			}

			reader := bufio.NewReader(resp.Body)
			type readResult struct {
				data string
				err  error
			}
			firstDelta := make(chan readResult, 1)
			go func() {
				var received strings.Builder
				for {
					line, readErr := reader.ReadString('\n')
					received.WriteString(line)
					if strings.Contains(received.String(), `"delta":"first"`) || readErr != nil {
						firstDelta <- readResult{data: received.String(), err: readErr}
						return
					}
				}
			}()
			var prefix string
			select {
			case result := <-firstDelta:
				if result.err != nil {
					t.Fatalf("stream ended before first delta: %v; body=%s", result.err, result.data)
				}
				prefix = result.data
			case <-time.After(time.Second):
				t.Fatal("first text delta was buffered until the upstream response completed")
			}
			if !upstreamStream.Load() {
				t.Fatal("facade did not request upstream streaming")
			}
			releaseOnce.Do(func() { close(release) })
			tail, err := io.ReadAll(reader)
			if err != nil {
				t.Fatal(err)
			}
			all := prefix + string(tail)
			if !strings.Contains(all, "response.completed") {
				t.Fatalf("stream did not complete: %s", all)
			}
		})
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

func TestResponsesFacadeAcceptsOfficialReasoningWithoutContent(t *testing.T) {
	reasoning := `{"type":"reasoning","id":"reasoning_item_1","summary":[{"type":"summary_text","text":"thinking"}],"status":"completed"}`
	reasoningWithNullContent := `{"type":"reasoning","id":"reasoning_item_1","summary":[{"type":"summary_text","text":"thinking"}],"content":null,"status":"completed"}`
	message := `{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"answer","annotations":[]}],"status":"completed"}`
	tests := []struct {
		name        string
		stream      bool
		contentType string
		output      string
	}{
		{name: "non-stream reasoning only", output: reasoning, contentType: "application/json"},
		{name: "non-stream reasoning with null content", output: reasoningWithNullContent, contentType: "application/json"},
		{name: "stream reasoning only", stream: true, output: reasoning, contentType: "text/event-stream"},
		{name: "stream reasoning and message", stream: true, output: reasoning + "," + message, contentType: "text/event-stream"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := `{"id":"resp_reasoning","object":"response","status":"completed","model":"grok-test","output":[` + test.output + `],"usage":{"input_tokens":1,"output_tokens":1}}`
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", test.contentType)
				if test.stream {
					_, _ = io.WriteString(w, `data: {"type":"response.completed","response":`+response+`}`+"\n\n")
					return
				}
				_, _ = io.WriteString(w, response)
			}))
			defer up.Close()

			s := New(log.New(io.Discard, "", 0))
			s.SetRoutes([]config.Route{facadeRoute("reasoning", "responses", "grok-test", "key", up.URL)})
			startPathTestServer(t, s)
			body := []byte(fmt.Sprintf(`{"input":"hi","stream":%t}`, test.stream))
			data, status := postFacade(t, s, "reasoning", body, "")
			if status != http.StatusOK || bytes.Contains(data, []byte("proxy_stream_error")) {
				t.Fatalf("status=%d body=%s", status, data)
			}
			if !bytes.Contains(data, []byte(`"type":"reasoning"`)) || bytes.Contains(data, []byte("content must be an array")) {
				t.Fatalf("reasoning item was rejected or lost: %s", data)
			}
		})
	}
}

func TestResponsesFacadeRejectsNonArrayReasoningContent(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp_bad_reasoning","object":"response","status":"completed","model":"model","output":[{"type":"reasoning","summary":[],"content":"invalid"}],"usage":{"input_tokens":0,"output_tokens":0}}`)
	}))
	defer up.Close()

	s := New(log.New(io.Discard, "", 0))
	s.SetRoutes([]config.Route{facadeRoute("bad-reasoning", "responses", "model", "key", up.URL)})
	startPathTestServer(t, s)
	data, status, headers := postFacadeResponse(t, s, "bad-reasoning", []byte(`{"input":"hi","stream":false}`), "")
	if status != http.StatusBadGateway || headers.Get("X-Should-Retry") != "false" || !bytes.Contains(data, []byte("must be an array when present")) {
		t.Fatalf("status=%d retry=%q body=%s", status, headers.Get("X-Should-Retry"), data)
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
		w.Header().Set("Content-Type", "text/event-stream")
		frames := []string{
			`{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"model":"deepseek-v4-pro","stop_reason":null,"usage":{"input_tokens":5,"cache_read_input_tokens":2,"output_tokens":1}}}`,
			`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"searching"}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig"}}`,
			`{"type":"content_block_stop","index":0}`,
			`{"type":"content_block_start","index":1,"content_block":{"type":"server_tool_use","id":"srv_1","name":"web_search","input":{}}}`,
			`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"query\":\"q\"}"}}`,
			`{"type":"content_block_stop","index":1}`,
			`{"type":"content_block_start","index":2,"content_block":{"type":"web_search_tool_result","tool_use_id":"srv_1","content":[{"type":"web_search_result","title":"Example","url":"https://example.test"},{"type":"web_search_result","title":"Docs","url":"https://docs.example.test/page"}]}}`,
			`{"type":"content_block_stop","index":2}`,
			`{"type":"content_block_start","index":3,"content_block":{"type":"text","text":""}}`,
			`{"type":"content_block_delta","index":3,"delta":{"type":"text_delta","text":"Example Domain"}}`,
			`{"type":"content_block_stop","index":3}`,
			`{"type":"content_block_start","index":4,"content_block":{"type":"tool_use","id":"call_1","name":"save","input":{}}}`,
			`{"type":"content_block_delta","index":4,"delta":{"type":"input_json_delta","partial_json":"{\"ok\":true}"}}`,
			`{"type":"content_block_stop","index":4}`,
			`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3,"server_tool_use":{"web_search_requests":1}}}`,
			`{"type":"message_stop"}`,
		}
		for _, frame := range frames {
			_, _ = fmt.Fprintf(w, "event: message\ndata: %s\n\n", frame)
		}
	}))
	defer up.Close()

	s := New(log.New(io.Discard, "", 0))
	s.SetRoutes([]config.Route{xAPIKeyFacadeRoute("deepseek-pro", "messages", "deepseek-v4-pro", "sk-deepseek", up.URL+"/anthropic")})
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
	if extractModel(gotBody) != "deepseek-v4-pro" || !bytes.Contains(gotBody, []byte(`"stream":true`)) {
		t.Fatalf("Messages request not converted: %s", gotBody)
	}
	for _, expected := range [][]byte{
		[]byte(`response.web_search_call.in_progress`),
		[]byte(`"type":"web_search_call"`),
		[]byte(`"url":"https://example.test"`),
		[]byte(`"url":"https://docs.example.test/page"`),
		[]byte(`response.reasoning_text.delta`),
		[]byte(`"delta":"searching"`),
		[]byte(`response.output_text.delta`),
		[]byte(`response.function_call_arguments.delta`),
		[]byte(`"arguments":"{\"ok\":true}"`),
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

func TestMessagesFacadeDefaultsToBearerAndHonorsExplicitXAPIKey(t *testing.T) {
	type observedRequest struct {
		authorization string
		xAPIKey       string
		path          string
	}
	seen := map[string]observedRequest{}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		model := extractModel(body)
		seen[model] = observedRequest{
			authorization: r.Header.Get("Authorization"),
			xAPIKey:       r.Header.Get("X-Api-Key"),
			path:          r.URL.Path,
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"model":%q,"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`, model)
	}))
	defer up.Close()

	routes, err := config.BuildRoutes([]config.Model{
		{ID: "messages-default", Model: "claude-default", BaseURL: up.URL + "/v1", APIBackend: "messages", APIKey: "bearer-key"},
		{ID: "messages-xapi", Model: "claude-xapi", BaseURL: up.URL + "/v1", APIBackend: "messages", APIKey: "xapi-key", AuthScheme: "x_api_key"},
	})
	if err != nil {
		t.Fatal(err)
	}
	s := New(log.New(io.Discard, "", 0))
	s.SetRoutes(routes)
	startPathTestServer(t, s)
	for _, channel := range []string{"messages-default", "messages-xapi"} {
		if data, status := postFacade(t, s, channel, []byte(`{"input":"hi","stream":false}`), ""); status != http.StatusOK {
			t.Fatalf("channel=%s status=%d body=%s", channel, status, data)
		}
	}
	if got := seen["claude-default"]; got.authorization != "Bearer bearer-key" || got.xAPIKey != "" || got.path != "/v1/messages" {
		t.Fatalf("default Messages auth/path mismatch: %+v", got)
	}
	if got := seen["claude-xapi"]; got.authorization != "" || got.xAPIKey != "xapi-key" || got.path != "/v1/messages" {
		t.Fatalf("explicit X-Api-Key auth/path mismatch: %+v", got)
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
	s.SetRoutes([]config.Route{xAPIKeyFacadeRoute("deepseek-pro", "messages", "deepseek-v4-pro", "sk-deepseek", up.URL+"/anthropic")})
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

func TestChatFacadeStreamsReasoningToolsAndSearchSources(t *testing.T) {
	var gotBody []byte
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		frames := []string{
			`{"id":"chat_1","object":"chat.completion.chunk","model":"grok-real","choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"thinking"},"finish_reason":null}]}`,
			`{"id":"chat_1","object":"chat.completion.chunk","model":"grok-real","choices":[{"index":0,"delta":{"content":"answer"},"finish_reason":null}]}`,
			`{"id":"chat_1","object":"chat.completion.chunk","model":"grok-real","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"sa","arguments":""}}]},"finish_reason":null}]}`,
			`{"id":"chat_1","object":"chat.completion.chunk","model":"grok-real","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":"ve","arguments":"{\"ok\":"}}]},"finish_reason":null}]}`,
			`{"id":"chat_1","object":"chat.completion.chunk","model":"grok-real","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"true}"}}]},"finish_reason":null}]}`,
			`{"id":"chat_1","object":"chat.completion.chunk","model":"grok-real","citations":["https://one.example.test/a","https://two.example.test/b"],"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":4,"completion_tokens":3,"completion_tokens_details":{"reasoning_tokens":1}}}`,
		}
		for _, frame := range frames {
			_, _ = fmt.Fprintf(w, "data: %s\n\n", frame)
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer up.Close()

	s := New(log.New(io.Discard, "", 0))
	s.SetRoutes([]config.Route{facadeRoute("grok-chat-stream", "chat_completions", "grok-real", "sk-chat", up.URL+"/v1")})
	startPathTestServer(t, s)
	body := []byte(`{"input":[{"role":"user","content":"search then save"}],"tools":[{"type":"web_search"},{"type":"function","name":"save","parameters":{"type":"object"}}],"stream":true}`)
	data, status := postFacade(t, s, "grok-chat-stream", body, "")
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, data)
	}
	for _, expected := range [][]byte{
		[]byte(`"stream":true`),
		[]byte(`"include_usage":true`),
		[]byte(`"search_parameters"`),
	} {
		if !bytes.Contains(gotBody, expected) {
			t.Fatalf("missing %s in Chat request: %s", expected, gotBody)
		}
	}
	for _, expected := range [][]byte{
		[]byte(`response.reasoning_text.delta`),
		[]byte(`"delta":"thinking"`),
		[]byte(`response.output_text.delta`),
		[]byte(`"delta":"answer"`),
		[]byte(`response.function_call_arguments.delta`),
		[]byte(`"arguments":"{\"ok\":true}"`),
		[]byte(`response.web_search_call.in_progress`),
		[]byte(`"url":"https://one.example.test/a"`),
		[]byte(`"url":"https://two.example.test/b"`),
		[]byte(`response.completed`),
	} {
		if !bytes.Contains(data, expected) {
			t.Fatalf("missing %s in translated Chat stream: %s", expected, data)
		}
	}
	if bytes.Contains(data, []byte("chat.completion.chunk")) {
		t.Fatalf("Chat-only chunk leaked to the Responses client: %s", data)
	}

	sourceCount := -1
	if err := scanSSEPayloads(bytes.NewReader(data), func(payload []byte) error {
		event, err := decodeJSONMap(payload)
		if err != nil {
			return err
		}
		if stringValue(event["type"]) != "response.output_item.done" {
			return nil
		}
		item, _ := event["item"].(map[string]any)
		if stringValue(item["type"]) != "web_search_call" {
			return nil
		}
		action, _ := item["action"].(map[string]any)
		sourceCount = len(anySlice(action["sources"]))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if sourceCount != 2 {
		t.Fatalf("web search source count=%d, want 2: %s", sourceCount, data)
	}
}

func TestMessagesCanonicalizationUsesOnlyRealSearchEvidence(t *testing.T) {
	withCitations := []byte(`{
		"id":"msg_sources","type":"message","role":"assistant","model":"model",
		"content":[{"type":"text","text":"answer","citations":[{"url":"https://one.example.test/a"},{"url":"https://two.example.test/b"}]}],
		"usage":{"input_tokens":1,"output_tokens":1,"server_tool_use":{"web_search_requests":1}}
	}`)
	result, err := canonicalFromMessages(withCitations, true, "query")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Output) != 2 {
		t.Fatalf("output=%d want search+message: %#v", len(result.Output), result.Output)
	}
	search := result.Output[0].(map[string]any)
	action, _ := search["action"].(map[string]any)
	if stringValue(search["type"]) != "web_search_call" || len(anySlice(action["sources"])) != 2 {
		t.Fatalf("citation sources were not converted to a web search result: %#v", result.Output)
	}

	withoutEvidence := []byte(`{
		"id":"msg_plain","type":"message","role":"assistant","model":"model",
		"content":[{"type":"text","text":"answer"}],
		"usage":{"input_tokens":1,"output_tokens":1}
	}`)
	result, err = canonicalFromMessages(withoutEvidence, true, "query")
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range result.Output {
		item, _ := raw.(map[string]any)
		if stringValue(item["type"]) == "web_search_call" {
			t.Fatalf("search activity was invented without upstream evidence: %#v", result.Output)
		}
	}
}

func TestMessagesCanonicalizationPreservesContentBlockOrder(t *testing.T) {
	result, err := canonicalFromMessages([]byte(`{
		"id":"msg_order",
		"type":"message",
		"role":"assistant",
		"model":"model",
		"content":[
			{"type":"text","text":"before"},
			{"type":"tool_use","id":"call_1","name":"save","input":{"ok":true}},
			{"type":"text","text":"after"}
		],
		"usage":{"input_tokens":1,"output_tokens":1}
	}`), false, "")
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

func TestFacadeRejectsEmptySuccessfulUpstreamEnvelope(t *testing.T) {
	for _, backend := range []string{"responses", "messages", "chat_completions"} {
		t.Run(backend, func(t *testing.T) {
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{}`)
			}))
			defer up.Close()

			s := New(log.New(io.Discard, "", 0))
			s.SetRoutes([]config.Route{facadeRoute("empty-envelope", backend, "model", "key", up.URL)})
			startPathTestServer(t, s)
			data, status := postFacade(t, s, "empty-envelope", []byte(`{"input":"hi","stream":false}`), "")
			if status != http.StatusBadGateway {
				t.Fatalf("status=%d body=%s", status, data)
			}
			if !bytes.Contains(data, []byte("invalid upstream")) {
				t.Fatalf("validation failure was not reported: %s", data)
			}
		})
	}
}

func TestFacadeRejectsHTMLSuccessWithConfigurationDiagnostic(t *testing.T) {
	for _, backend := range []string{"responses", "messages", "chat_completions"} {
		t.Run(backend, func(t *testing.T) {
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				_, _ = io.WriteString(w, `<html>private-home-page-marker</html>`)
			}))
			defer up.Close()

			s := New(log.New(io.Discard, "", 0))
			s.SetRoutes([]config.Route{facadeRoute("html-"+backend, backend, "model", "key", up.URL)})
			startPathTestServer(t, s)
			data, status, headers := postFacadeResponse(t, s, "html-"+backend, []byte(`{"input":"hi","stream":false}`), "")
			if status != http.StatusBadGateway || headers.Get("X-Should-Retry") != "false" {
				t.Fatalf("status=%d retry=%q body=%s", status, headers.Get("X-Should-Retry"), data)
			}
			if !bytes.Contains(data, []byte("returned HTML")) || !bytes.Contains(data, []byte("base_url")) || !bytes.Contains(data, []byte("api_backend")) {
				t.Fatalf("configuration diagnostic missing: %s", data)
			}
			if bytes.Contains(data, []byte("private-home-page-marker")) {
				t.Fatalf("HTML response body leaked into diagnostic: %s", data)
			}
		})
	}
}

func TestFacadeMarksUpstreamTransportFailureRetryable(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	origin := up.URL
	up.Close()

	s := New(log.New(io.Discard, "", 0))
	s.SetRoutes([]config.Route{facadeRoute("transport-failure", "responses", "model", "key", origin)})
	startPathTestServer(t, s)
	data, status, headers := postFacadeResponse(t, s, "transport-failure", []byte(`{"input":"hi","stream":false}`), "")
	if status != http.StatusBadGateway || headers.Get("X-Should-Retry") != "true" {
		t.Fatalf("status=%d retry=%q body=%s", status, headers.Get("X-Should-Retry"), data)
	}
}

func TestFacadePreservesUpstreamRetryVeto(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Should-Retry", "false")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"error":{"message":"maintenance"}}`)
	}))
	defer up.Close()

	s := New(log.New(io.Discard, "", 0))
	s.SetRoutes([]config.Route{facadeRoute("upstream-veto", "responses", "model", "key", up.URL)})
	startPathTestServer(t, s)
	data, status, headers := postFacadeResponse(t, s, "upstream-veto", []byte(`{"input":"hi","stream":false}`), "")
	if status != http.StatusServiceUnavailable || headers.Get("X-Should-Retry") != "false" || !bytes.Contains(data, []byte("maintenance")) {
		t.Fatalf("status=%d retry=%q body=%s", status, headers.Get("X-Should-Retry"), data)
	}
}

func TestFacadeRejectsNullResponsesOutputItemWithoutStreaming(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp_bad","object":"response","status":"completed","model":"model","output":[null],"usage":{"input_tokens":0,"output_tokens":0}}`)
	}))
	defer up.Close()

	s := New(log.New(io.Discard, "", 0))
	s.SetRoutes([]config.Route{facadeRoute("null-output", "responses", "model", "key", up.URL)})
	startPathTestServer(t, s)
	data, status, headers := postFacadeResponse(t, s, "null-output", []byte(`{"input":"hi","stream":false}`), "")
	if status != http.StatusBadGateway || headers.Get("X-Should-Retry") != "false" || !bytes.Contains(data, []byte("output[0] must be an object")) {
		t.Fatalf("status=%d retry=%q body=%s", status, headers.Get("X-Should-Retry"), data)
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

func TestTranslatedFacadeSurfacesPrematureSSEEnd(t *testing.T) {
	tests := []struct {
		backend string
		body    string
		want    string
	}{
		{
			backend: "messages",
			body: `data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"model":"model","usage":{"input_tokens":1,"output_tokens":0}}}` + "\n\n" +
				`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
				`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"partial"}}` + "\n\n",
			want: "without message_stop",
		},
		{
			backend: "chat_completions",
			body:    `data: {"id":"chat_1","object":"chat.completion.chunk","model":"model","choices":[{"index":0,"delta":{"content":"partial"},"finish_reason":null}]}` + "\n\n",
			want:    "without a terminal chunk",
		},
	}
	for _, test := range tests {
		t.Run(test.backend, func(t *testing.T) {
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, test.body)
			}))
			defer up.Close()
			s := New(log.New(io.Discard, "", 0))
			s.SetRoutes([]config.Route{facadeRoute("truncated-translated", test.backend, "model", "key", up.URL)})
			startPathTestServer(t, s)
			data, status := postFacade(t, s, "truncated-translated", []byte(`{"input":"hi","stream":true}`), "")
			if status != http.StatusOK {
				t.Fatalf("status=%d body=%s", status, data)
			}
			if !bytes.Contains(data, []byte("event: error")) || !bytes.Contains(data, []byte(test.want)) {
				t.Fatalf("truncated %s stream was silent: %s", test.backend, data)
			}
			if bytes.Contains(data, []byte("response.completed")) {
				t.Fatalf("truncated %s stream was marked completed: %s", test.backend, data)
			}
		})
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
