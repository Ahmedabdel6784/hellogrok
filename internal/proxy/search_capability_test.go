package proxy

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hellowind777/hellogrok/internal/config"
)

func TestDetectSearchCapabilitiesResponsesCachesByCredentialIdentity(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/v1/responses" {
			t.Errorf("probe path = %q", r.URL.Path)
		}
		if auth := r.Header.Get("Authorization"); auth != "Bearer first-secret" && auth != "Bearer second-secret" {
			t.Errorf("probe auth = %q", auth)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode probe: %v", err)
			return
		}
		tools := anySlice(body["tools"])
		if len(tools) != 1 {
			t.Errorf("probe tools = %#v", body["tools"])
			return
		}
		toolType := stringValue(tools[0].(map[string]any)["type"])
		w.Header().Set("Content-Type", "application/json")
		if toolType == "x_search" {
			_, _ = io.WriteString(w, `{"id":"resp_x","output":[{"type":"custom_tool_call","id":"xs_1","name":"x_keyword_search","input":"{\"query\":\"xai\"}"}]}`)
			return
		}
		if toolType != "web_search" {
			t.Errorf("unexpected probe tool %q", toolType)
		}
		_, _ = io.WriteString(w, `{"id":"resp_web","output":[{"type":"web_search_call","id":"ws_1","status":"completed","action":{"type":"search","query":"golang"}}]}`)
	}))
	defer upstream.Close()

	server := New(log.New(io.Discard, "", 0))
	route := facadeRoute("grok-relay", "responses", "grok-4.5", "first-secret", upstream.URL+"/v1")
	cachePath := SearchCapabilityCachePath(t.TempDir())

	first := server.DetectSearchCapabilities(context.Background(), []config.Route{route}, cachePath, true)[route.ChannelID]
	if first.WebSearch.State != CapabilitySupported || first.XSearch.State != CapabilitySupported ||
		first.WebSearch.Source != "probe" || first.XSearch.Source != "probe" {
		t.Fatalf("first detection = %+v", first)
	}
	if calls.Load() != 2 {
		t.Fatalf("probe calls = %d, want 2", calls.Load())
	}

	second := server.DetectSearchCapabilities(context.Background(), []config.Route{route}, cachePath, true)[route.ChannelID]
	if second.WebSearch.Source != "cache" || second.XSearch.Source != "cache" || calls.Load() != 2 {
		t.Fatalf("cached detection = %+v calls=%d", second, calls.Load())
	}

	cacheData, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"first-secret", upstream.URL, "official Go programming language", "official xAI account"} {
		if strings.Contains(string(cacheData), secret) {
			t.Fatalf("cache leaked route or request material %q: %s", secret, cacheData)
		}
	}

	route.APIKey = "second-secret"
	third := server.DetectSearchCapabilities(context.Background(), []config.Route{route}, cachePath, true)[route.ChannelID]
	if third.WebSearch.Source != "probe" || third.XSearch.Source != "probe" || calls.Load() != 4 {
		t.Fatalf("credential change did not invalidate cache: %+v calls=%d", third, calls.Load())
	}
	info, err := os.Stat(cachePath)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("cache permissions = %v", info.Mode().Perm())
	}
}

func TestDetectSearchCapabilitiesAdaptsMessagesAndChatWebSearch(t *testing.T) {
	tests := []struct {
		backend string
		check   func(*testing.T, map[string]any)
		body    string
	}{
		{
			backend: "messages",
			check: func(t *testing.T, body map[string]any) {
				tools := anySlice(body["tools"])
				if len(tools) != 1 || stringValue(tools[0].(map[string]any)["type"]) != "web_search_20250305" {
					t.Fatalf("Messages probe tools = %#v", body["tools"])
				}
			},
			body: `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"server_tool_use","id":"ws_1","name":"web_search","input":{"query":"golang"}},{"type":"web_search_tool_result","tool_use_id":"ws_1","content":[{"type":"web_search_result","url":"https://go.dev","title":"Go"}]}]}`,
		},
		{
			backend: "chat_completions",
			check: func(t *testing.T, body map[string]any) {
				parameters, _ := body["search_parameters"].(map[string]any)
				if stringValue(parameters["mode"]) != "on" {
					t.Fatalf("Chat probe search_parameters = %#v", body["search_parameters"])
				}
			},
			body: `{"id":"chat_1","choices":[{"message":{"role":"assistant","content":"Go","annotations":[{"type":"url_citation","url":"https://go.dev"}]}}]}`,
		},
	}
	for _, test := range tests {
		t.Run(test.backend, func(t *testing.T) {
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Errorf("decode probe: %v", err)
					return
				}
				test.check(t, body)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, test.body)
			}))
			defer upstream.Close()

			server := New(log.New(io.Discard, "", 0))
			route := facadeRoute("grok-"+test.backend, test.backend, "grok-4.5", "secret", upstream.URL+"/v1")
			report := server.DetectSearchCapabilities(
				context.Background(), []config.Route{route}, filepath.Join(t.TempDir(), "capabilities.json"), true,
			)[route.ChannelID]
			if report.WebSearch.State != CapabilitySupported || report.XSearch.State != CapabilityUnsupported ||
				report.XSearch.Source != "protocol-boundary" || calls.Load() != 1 {
				t.Fatalf("report=%+v calls=%d", report, calls.Load())
			}
			if test.backend == "chat_completions" && report.WebSearch.ChatDialect != config.ChatSearchDialectSearchParameters {
				t.Fatalf("Chat dialect = %q, want %q", report.WebSearch.ChatDialect, config.ChatSearchDialectSearchParameters)
			}
		})
	}
}

func TestDetectSearchCapabilitiesChatFallsBackToWebSearchOptionsAndCachesDialect(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode probe: %v", err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if _, exists := body["search_parameters"]; exists {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"message":"Unknown field search_parameters"}}`)
			return
		}
		if _, exists := body["web_search_options"]; !exists {
			t.Errorf("fallback request has neither Chat search dialect: %#v", body)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_, _ = io.WriteString(w, `{"id":"chat_1","choices":[{"message":{"role":"assistant","content":"Go","annotations":[{"type":"url_citation","url":"https://go.dev"}]}}]}`)
	}))
	defer upstream.Close()

	server := New(log.New(io.Discard, "", 0))
	route := facadeRoute("grok2api-chat", "chat_completions", "grok-4.5", "secret", upstream.URL+"/v1")
	cachePath := SearchCapabilityCachePath(t.TempDir())
	first := server.DetectSearchCapabilities(context.Background(), []config.Route{route}, cachePath, true)[route.ChannelID]
	if first.WebSearch.State != CapabilitySupported ||
		first.WebSearch.ChatDialect != config.ChatSearchDialectWebSearchOptions || calls.Load() != 2 {
		t.Fatalf("first report=%+v calls=%d", first, calls.Load())
	}
	second := server.DetectSearchCapabilities(context.Background(), []config.Route{route}, cachePath, true)[route.ChannelID]
	if second.WebSearch.State != CapabilitySupported || second.WebSearch.Source != "cache" ||
		second.WebSearch.ChatDialect != config.ChatSearchDialectWebSearchOptions || calls.Load() != 2 {
		t.Fatalf("cached report=%+v calls=%d", second, calls.Load())
	}
}

func TestProbeSearchCapabilityClassifiesEvidenceAndFailuresConservatively(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		want   CapabilityState
	}{
		{name: "structured evidence", status: 200, body: `{"output":[{"type":"web_search_call","id":"ws_1","status":"completed","action":{"type":"search","query":"go"}}]}`, want: CapabilitySupported},
		{name: "success without evidence", status: 200, body: `{"output":[{"type":"message","content":[{"type":"output_text","text":"guess"}]}]}`, want: CapabilityUnknown},
		{name: "unknown tool", status: 400, body: `{"error":{"message":"Unknown tool web_search"}}`, want: CapabilityUnsupported},
		{name: "unprocessable tool", status: 422, body: `{"error":{"message":"web_search is not supported"}}`, want: CapabilityUnsupported},
		{name: "authentication remains unknown", status: 401, body: `{"error":{"message":"Unknown tool web_search"}}`, want: CapabilityUnknown},
		{name: "rate limit remains unknown", status: 429, body: `{"error":{"message":"web_search unavailable"}}`, want: CapabilityUnknown},
		{name: "server failure remains unknown", status: 500, body: `{"error":{"message":"web_search failed"}}`, want: CapabilityUnknown},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(test.status)
				_, _ = io.WriteString(w, test.body)
			}))
			defer upstream.Close()
			server := New(log.New(io.Discard, "", 0))
			route := facadeRoute("grok-probe", "responses", "grok-4.5", "secret", upstream.URL+"/v1")
			got := server.probeSearchCapability(context.Background(), route, searchCapabilityWeb)
			if got.State != test.want {
				t.Fatalf("state=%s source=%s want %s", got.State, got.Source, test.want)
			}
		})
	}
}

func TestDetectSearchCapabilitiesSkipsRoutesWithoutProbeCredential(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		calls.Add(1)
	}))
	defer upstream.Close()
	server := New(log.New(io.Discard, "", 0))
	route := facadeRoute("grok-dynamic", "responses", "grok-4.5", "", upstream.URL+"/v1")
	route.DynamicAuth = true
	report := server.DetectSearchCapabilities(context.Background(), []config.Route{route}, SearchCapabilityCachePath(t.TempDir()), true)[route.ChannelID]
	if report.WebSearch.State != CapabilityUnknown || report.WebSearch.Source != "no-static-credential" ||
		report.XSearch.State != CapabilityUnknown || calls.Load() != 0 {
		t.Fatalf("report=%+v calls=%d", report, calls.Load())
	}
}

func TestSearchCapabilityCacheUsesStateSpecificTTL(t *testing.T) {
	if capabilityTTL(CapabilitySupported) != 24*time.Hour ||
		capabilityTTL(CapabilityUnsupported) != 6*time.Hour ||
		capabilityTTL(CapabilityUnknown) != 10*time.Minute {
		t.Fatal("capability TTLs changed")
	}
	cache, err := newSearchCapabilityCache()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	cache.set("supported", searchCapabilityWeb, SearchToolCapability{State: CapabilitySupported}, now.Add(-25*time.Hour))
	cache.set("unsupported", searchCapabilityWeb, SearchToolCapability{State: CapabilityUnsupported}, now.Add(-5*time.Hour))
	cache.set("unknown", searchCapabilityWeb, SearchToolCapability{State: CapabilityUnknown}, now.Add(-11*time.Minute))
	if _, ok := cache.get("supported", searchCapabilityWeb, now); ok {
		t.Fatal("expired supported result was accepted")
	}
	if value, ok := cache.get("unsupported", searchCapabilityWeb, now); !ok || value.State != CapabilityUnsupported {
		t.Fatalf("fresh unsupported result=%+v ok=%t", value, ok)
	}
	if _, ok := cache.get("unknown", searchCapabilityWeb, now); ok {
		t.Fatal("expired unknown result was accepted")
	}
}

func TestStructuredSearchEvidenceAcceptsResponsesSSEAndXCalls(t *testing.T) {
	webSSE := []byte("event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"web_search_call\",\"id\":\"ws_1\",\"status\":\"completed\"}}\n\n")
	if !hasStructuredSearchEvidence(webSSE, "text/event-stream", searchCapabilityWeb) {
		t.Fatal("Responses SSE web_search evidence was missed")
	}
	xJSON := []byte(`{"output":[{"type":"custom_tool_call","id":"xs_1","name":"x_semantic_search","input":"{}"}]}`)
	if !hasStructuredSearchEvidence(xJSON, "application/json", searchCapabilityX) {
		t.Fatal("Responses x_search evidence was missed")
	}
}
