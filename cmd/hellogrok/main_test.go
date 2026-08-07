package main

import (
	"bytes"
	"context"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hellowind777/hellogrok/internal/appinfo"
	"github.com/hellowind777/hellogrok/internal/cfgpatch"
	"github.com/hellowind777/hellogrok/internal/config"
	"github.com/hellowind777/hellogrok/internal/proxy"
)

func TestUsageIncludesApplicationVersion(t *testing.T) {
	var output bytes.Buffer
	printUsage(&output)
	if !strings.Contains(output.String(), "hellogrok "+appinfo.Version) ||
		!strings.Contains(output.String(), "version               print the application version") {
		t.Fatalf("usage does not expose the application version:\n%s", output.String())
	}
}

func TestRouteAuthStatusIgnoresNonCredentialHeaders(t *testing.T) {
	tests := []struct {
		name  string
		route config.Route
		want  string
	}{
		{name: "ordinary header", route: config.Route{ExtraHeaders: map[string]string{"X-Tenant": "one"}}, want: "missing"},
		{name: "api key", route: config.Route{APIKey: "secret"}, want: "channel-owned"},
		{name: "authorization header", route: config.Route{ExtraHeaders: map[string]string{"authorization": "Bearer secret"}}, want: "channel-owned"},
		{name: "x api key header", route: config.Route{ExtraHeaders: map[string]string{"X-API-KEY": "secret"}}, want: "channel-owned"},
		{name: "empty auth header", route: config.Route{ExtraHeaders: map[string]string{"Authorization": "  "}}, want: "missing"},
		{name: "dynamic provider", route: config.Route{DynamicAuth: true}, want: "auth-provider"},
		{name: "static wins", route: config.Route{APIKey: "secret", DynamicAuth: true}, want: "channel-owned"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := routeAuthStatus(test.route); got != test.want {
				t.Fatalf("routeAuthStatus() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestSecondInstanceDoesNotRestoreActiveInstanceConfig(t *testing.T) {
	dir := t.TempDir()
	grokHome := filepath.Join(dir, "grok")
	dataDir := filepath.Join(dir, "data")
	if err := os.MkdirAll(grokHome, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GROK_HOME", grokHome)
	configPath := filepath.Join(grokHome, "config.toml")
	statePath := filepath.Join(dataDir, "config_rewrite_state.json")
	original := "[model.one]\nbase_url = \"https://one.example/v1\"\napi_key = \"test-key\"\n"
	if err := os.WriteFile(configPath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := cfgpatch.ApplyTargets(configPath, statePath, []cfgpatch.Target{{ID: "one"}}); err != nil {
		t.Fatal(err)
	}
	patchedBefore, _ := os.ReadFile(configPath)
	stateBefore, _ := os.ReadFile(statePath)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	logger := log.New(io.Discard, "", 0)
	server := proxy.New(logger)
	server.PathAddr = listener.Addr().String()
	app := &App{logger: logger, dataDir: dataDir, server: server}
	if err := app.Start(); err == nil {
		t.Fatal("second instance unexpectedly started on an occupied address")
	}
	if err := app.Stop(); err != nil {
		t.Fatalf("stopping non-owning second instance: %v", err)
	}

	patchedAfter, _ := os.ReadFile(configPath)
	stateAfter, _ := os.ReadFile(statePath)
	if !bytes.Equal(patchedAfter, patchedBefore) {
		t.Fatalf("second instance restored active config\nbefore: %q\nafter:  %q", patchedBefore, patchedAfter)
	}
	if !bytes.Equal(stateAfter, stateBefore) {
		t.Fatal("second instance start/stop changed active rewrite state")
	}
}

func TestAppRemembersProxyEnabledStateAcrossInstances(t *testing.T) {
	dataDir := t.TempDir()
	first := &App{dataDir: dataDir}
	if enabled, err := first.ProxyEnabledOnLaunch(); err != nil || !enabled {
		t.Fatalf("initial enabled=%v err=%v", enabled, err)
	}
	if err := first.SetProxyEnabledOnLaunch(false); err != nil {
		t.Fatal(err)
	}
	second := &App{dataDir: dataDir}
	if enabled, err := second.ProxyEnabledOnLaunch(); err != nil || enabled {
		t.Fatalf("remembered disabled=%v err=%v", enabled, err)
	}
	if err := second.SetProxyEnabledOnLaunch(true); err != nil {
		t.Fatal(err)
	}
	if enabled, err := first.ProxyEnabledOnLaunch(); err != nil || !enabled {
		t.Fatalf("remembered enabled=%v err=%v", enabled, err)
	}
}

func TestAppStartRejectsEmptyCustomRouteSet(t *testing.T) {
	dir := t.TempDir()
	grokHome := filepath.Join(dir, "grok")
	if err := os.MkdirAll(grokHome, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GROK_HOME", grokHome)
	if err := os.WriteFile(filepath.Join(grokHome, "config.toml"), []byte("[model.official]\nmodel = \"grok-4.5\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	server := proxy.New(log.New(io.Discard, "", 0))
	server.PathAddr = "127.0.0.1:0"
	app := &App{logger: log.New(io.Discard, "", 0), dataDir: filepath.Join(dir, "data"), server: server}
	err := app.Start()
	if err == nil || !strings.Contains(err.Error(), "no explicit custom model endpoints") {
		t.Fatalf("start error = %v", err)
	}
	if app.IsRunning() {
		t.Fatal("app remained running without a custom route")
	}
}

func TestAppStartRejectsUnrecoverableProxyURLAmongValidRoutes(t *testing.T) {
	dir := t.TempDir()
	grokHome := filepath.Join(dir, "grok")
	dataDir := filepath.Join(dir, "data")
	if err := os.MkdirAll(grokHome, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GROK_HOME", grokHome)
	configPath := filepath.Join(grokHome, "config.toml")
	original := "[model.stale]\nbase_url = \"http://127.0.0.1:18787/c/stale\"\napi_key = \"test-key\"\n\n" +
		"[model.valid]\nbase_url = \"https://valid.example/v1\"\napi_key = \"test-key\"\n"
	if err := os.WriteFile(configPath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	server := proxy.New(log.New(io.Discard, "", 0))
	server.PathAddr = "127.0.0.1:0"
	app := &App{logger: log.New(io.Discard, "", 0), dataDir: dataDir, server: server}

	err := app.Start()
	if err == nil || !strings.Contains(err.Error(), "no restorable origin is available") {
		t.Fatalf("start error = %v", err)
	}
	if app.IsRunning() {
		t.Fatal("app remained running with an unrecoverable proxy URL")
	}
	current, _ := os.ReadFile(configPath)
	if string(current) != original {
		t.Fatalf("config changed after stale proxy rejection: %q", current)
	}
	if _, err := os.Stat(cfgpatch.StatePath(dataDir)); !os.IsNotExist(err) {
		t.Fatalf("rewrite state unexpectedly created: %v", err)
	}
}

func TestAppStartStopLifecycleRestoresConfigExactly(t *testing.T) {
	dir := t.TempDir()
	grokHome := filepath.Join(dir, "grok")
	dataDir := filepath.Join(dir, "data")
	if err := os.MkdirAll(grokHome, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GROK_HOME", grokHome)
	configPath := filepath.Join(grokHome, "config.toml")
	original := strings.Join([]string{
		"[subagents.models]",
		`general-purpose = "one"`,
		"",
		"[features]",
		"backend_tools = false",
		"web_fetch = false",
		"",
		"[model.one]",
		`base_url = "https://api.example.test/v1"`,
		`api_key = "test-key"`,
		`api_backend = "chat_completions"`,
		"",
	}, "\n")
	if err := os.WriteFile(configPath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	server := proxy.New(log.New(io.Discard, "", 0))
	server.PathAddr = "127.0.0.1:0"
	app := &App{
		logger:  log.New(io.Discard, "", 0),
		dataDir: dataDir,
		server:  server,
	}
	if err := app.Start(); err != nil {
		t.Fatal(err)
	}
	if !app.IsRunning() {
		t.Fatal("app did not enter running state")
	}
	patched, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		`base_url = "http://127.0.0.1:18787/c/one"`,
		`api_backend = "responses"`,
		"supports_backend_search = false",
		"backend_tools = true",
		"web_fetch = true",
		"enabled = true",
	} {
		if !strings.Contains(string(patched), expected) {
			t.Fatalf("running config missing %q:\n%s", expected, patched)
		}
	}
	if _, err := os.Stat(cfgpatch.StatePath(dataDir)); err != nil {
		t.Fatalf("rewrite state missing while running: %v", err)
	}

	if err := app.Stop(); err != nil {
		t.Fatal(err)
	}
	if app.IsRunning() {
		t.Fatal("app remained in running state after stop")
	}
	restored, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(restored) != original {
		t.Fatalf("lifecycle restore was not byte-exact\nwant: %q\ngot:  %q", original, restored)
	}
	if _, err := os.Stat(cfgpatch.StatePath(dataDir)); !os.IsNotExist(err) {
		t.Fatalf("rewrite state remains after stop: %v", err)
	}
}

func TestAppStartRejectsCCSwitchTakeoverBeforeRecovery(t *testing.T) {
	dir := t.TempDir()
	grokHome := filepath.Join(dir, "grok")
	dataDir := filepath.Join(dir, "data")
	if err := os.MkdirAll(grokHome, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GROK_HOME", grokHome)
	configPath := filepath.Join(grokHome, "config.toml")
	statePath := cfgpatch.StatePath(dataDir)
	original := "[models]\ndefault = \"one\"\n\n[model.one]\n" +
		"base_url = \"https://one.example/v1\"\napi_key = \"test-key\"\n"
	if err := os.WriteFile(configPath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := cfgpatch.ApplyTargets(configPath, statePath, []cfgpatch.Target{{ID: "one"}}); err != nil {
		t.Fatal(err)
	}
	stateBefore, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	ccSwitchConfig := "[models]\ndefault = \"one\"\n\n[model.one]\n" +
		"base_url = \"http://127.0.0.1:15721/grokbuild/v1\"\n" +
		"api_key = \"PROXY_MANAGED\"\napi_backend = \"responses\"\n"
	if err := os.WriteFile(configPath, []byte(ccSwitchConfig), 0o600); err != nil {
		t.Fatal(err)
	}

	server := proxy.New(log.New(io.Discard, "", 0))
	server.PathAddr = "127.0.0.1:0"
	app := &App{logger: log.New(io.Discard, "", 0), dataDir: dataDir, server: server}
	err = app.Start()
	if err == nil || !strings.Contains(err.Error(), "CC Switch") {
		t.Fatalf("start error = %v", err)
	}
	if app.IsRunning() {
		t.Fatal("app started while CC Switch owned the Grok config")
	}
	current, _ := os.ReadFile(configPath)
	if string(current) != ccSwitchConfig {
		t.Fatalf("CC Switch config changed: %q", current)
	}
	stateAfter, _ := os.ReadFile(statePath)
	if !bytes.Equal(stateAfter, stateBefore) {
		t.Fatal("recovery state changed before CC Switch takeover was released")
	}
}

func TestAppStopWaitsForCCSwitchThenRestoresInSafeOrder(t *testing.T) {
	dir := t.TempDir()
	grokHome := filepath.Join(dir, "grok")
	dataDir := filepath.Join(dir, "data")
	if err := os.MkdirAll(grokHome, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GROK_HOME", grokHome)
	configPath := filepath.Join(grokHome, "config.toml")
	original := "[models]\ndefault = \"one\"\n\n[model.one]\n" +
		"base_url = \"https://one.example/v1\"\napi_key = \"test-key\"\n"
	if err := os.WriteFile(configPath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	server := proxy.New(log.New(io.Discard, "", 0))
	server.PathAddr = "127.0.0.1:0"
	app := &App{logger: log.New(io.Discard, "", 0), dataDir: dataDir, server: server}
	if err := app.Start(); err != nil {
		t.Fatal(err)
	}
	patched, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	ccSwitchConfig := strings.Replace(string(patched),
		`base_url = "http://127.0.0.1:18787/c/one"`,
		`base_url = "http://127.0.0.1:15721/grokbuild/v1"`, 1)
	ccSwitchConfig = strings.Replace(ccSwitchConfig, `api_key = "test-key"`, `api_key = "PROXY_MANAGED"`, 1)
	if err := os.WriteFile(configPath, []byte(ccSwitchConfig), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := app.Stop(); err == nil || !strings.Contains(err.Error(), "CC Switch") {
		t.Fatalf("stop during CC Switch takeover error = %v", err)
	}
	if !app.IsRunning() {
		t.Fatal("proxy stopped before CC Switch restored its hellogrok backup")
	}
	if _, err := os.Stat(cfgpatch.StatePath(dataDir)); err != nil {
		t.Fatalf("recovery state was lost during CC Switch takeover: %v", err)
	}
	current, _ := os.ReadFile(configPath)
	if string(current) != ccSwitchConfig {
		t.Fatal("stop attempt overwrote the active CC Switch config")
	}

	// CC Switch stops first and restores the live snapshot it captured while
	// hellogrok was active. hellogrok can then restore the true original.
	if err := os.WriteFile(configPath, patched, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := app.Stop(); err != nil {
		t.Fatal(err)
	}
	if app.IsRunning() {
		t.Fatal("app remained running after the safe stop order")
	}
	restored, _ := os.ReadFile(configPath)
	if string(restored) != original {
		t.Fatalf("safe stop order did not restore original\nwant: %q\ngot:  %q", original, restored)
	}
}

func TestAppStopRelinquishesCompleteExternalProviderReplacement(t *testing.T) {
	dir := t.TempDir()
	grokHome := filepath.Join(dir, "grok")
	dataDir := filepath.Join(dir, "data")
	if err := os.MkdirAll(grokHome, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GROK_HOME", grokHome)
	configPath := filepath.Join(grokHome, "config.toml")
	original := "[model.one]\nbase_url = \"https://one.example/v1\"\napi_key = \"test-key\"\n"
	if err := os.WriteFile(configPath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	server := proxy.New(log.New(io.Discard, "", 0))
	server.PathAddr = "127.0.0.1:0"
	app := &App{logger: log.New(io.Discard, "", 0), dataDir: dataDir, server: server}
	if err := app.Start(); err != nil {
		t.Fatal(err)
	}
	external := "[model.two]\nbase_url = \"https://two.example/v1\"\napi_key = \"new-key\"\n"
	if err := os.WriteFile(configPath, []byte(external), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := app.Stop(); err != nil {
		t.Fatal(err)
	}
	if app.IsRunning() {
		t.Fatal("app remained running after a complete external provider replacement")
	}
	current, _ := os.ReadFile(configPath)
	if string(current) != external {
		t.Fatalf("external provider config was overwritten: %q", current)
	}
	if _, err := os.Stat(cfgpatch.StatePath(dataDir)); !os.IsNotExist(err) {
		t.Fatalf("obsolete recovery state remains: %v", err)
	}
}

func TestAppStopKeepsServingWhenConflictStillReferencesHellogrok(t *testing.T) {
	dir := t.TempDir()
	grokHome := filepath.Join(dir, "grok")
	dataDir := filepath.Join(dir, "data")
	if err := os.MkdirAll(grokHome, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GROK_HOME", grokHome)
	configPath := filepath.Join(grokHome, "config.toml")
	original := "[model.one]\nbase_url = \"https://one.example/v1\"\napi_key = \"test-key\"\n"
	if err := os.WriteFile(configPath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	server := proxy.New(log.New(io.Discard, "", 0))
	server.PathAddr = "127.0.0.1:0"
	app := &App{logger: log.New(io.Discard, "", 0), dataDir: dataDir, server: server}
	if err := app.Start(); err != nil {
		t.Fatal(err)
	}
	patched, _ := os.ReadFile(configPath)
	conflicted := strings.Replace(string(patched), `api_backend = "responses"`, `api_backend = "chat_completions"`, 1)
	if err := os.WriteFile(configPath, []byte(conflicted), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := app.Stop(); err == nil {
		t.Fatal("stop succeeded while an unresolved hellogrok route remained")
	}
	if !app.IsRunning() {
		t.Fatal("proxy stopped while config still referenced it")
	}
	if err := os.WriteFile(configPath, patched, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := app.Stop(); err != nil {
		t.Fatal(err)
	}
}

func TestEnsureFacadeIdleRejectsOccupiedAddress(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := ensureFacadeIdle(address); err == nil {
		_ = listener.Close()
		t.Fatal("occupied facade address was treated as idle")
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	if err := ensureFacadeIdle(address); err != nil {
		t.Fatalf("released facade address remained busy: %v", err)
	}
}

func TestResolveSearchRoutesExplicitClientModelOverridesEveryConversationRoute(t *testing.T) {
	var detected []config.Route
	var probeX bool
	app := &App{
		logger:  log.New(io.Discard, "", 0),
		dataDir: t.TempDir(),
		detectSearchCapabilities: func(_ context.Context, routes []config.Route, _ string, includeX bool) map[string]proxy.SearchCapabilities {
			detected = append([]config.Route(nil), routes...)
			probeX = includeX
			return map[string]proxy.SearchCapabilities{
				"deepseek-v4-flash": {
					WebSearch: proxy.SearchToolCapability{
						State: proxy.CapabilitySupported, Source: "probe",
						ChatDialect: config.ChatSearchDialectWebSearchOptions,
					},
				},
			}
		},
	}
	routes := []config.Route{
		{ChannelID: "grok-custom", WireModel: "grok-4.5", APIBackend: "responses", SupportsBackendSearch: true, BackendSearchSet: true},
		{ChannelID: "deepseek-v4-flash", WireModel: "deepseek-v4-flash", APIBackend: "chat_completions", SupportsBackendSearch: true, BackendSearchSet: true},
		{ChannelID: "plain", WireModel: "plain", APIBackend: "chat_completions"},
	}
	effective := app.resolveSearchRoutes(context.Background(), routes, config.WebSearchSelection{
		Model: "deepseek-v4-flash", Explicit: true, Source: "config",
	})
	for _, route := range effective {
		if route.SupportsBackendSearch || !route.HostedSearchKnown || route.HostedWebSearch || route.HostedXSearch {
			t.Fatalf("explicit client-search model did not force a client route: %+v", route)
		}
	}
	if probeX || len(detected) != 1 || detected[0].ChannelID != "deepseek-v4-flash" {
		t.Fatalf("validation routes=%+v probeX=%t", detected, probeX)
	}
	if effective[1].HostedChatSearchDialect != config.ChatSearchDialectWebSearchOptions {
		t.Fatalf("client search model lost detected Chat dialect: %+v", effective[1])
	}
}

func TestResolveSearchRoutesAutoDetectsOnlyMissingGrokDeclarations(t *testing.T) {
	var detected []config.Route
	app := &App{
		logger:  log.New(io.Discard, "", 0),
		dataDir: t.TempDir(),
		detectSearchCapabilities: func(_ context.Context, routes []config.Route, _ string, includeX bool) map[string]proxy.SearchCapabilities {
			if !includeX {
				t.Fatal("automatic Grok detection omitted x_search")
			}
			detected = append([]config.Route(nil), routes...)
			return map[string]proxy.SearchCapabilities{
				"grok-auto": {
					WebSearch: proxy.SearchToolCapability{
						State: proxy.CapabilitySupported, Source: "probe",
						ChatDialect: config.ChatSearchDialectWebSearchOptions,
					},
					XSearch: proxy.SearchToolCapability{State: proxy.CapabilityUnsupported, Source: "probe"},
				},
			}
		},
	}
	routes := []config.Route{
		{ChannelID: "grok-auto", WireModel: "grok-4.5", APIBackend: "chat_completions"},
		{ChannelID: "gpt-auto", WireModel: "gpt-5.6", APIBackend: "responses"},
		{ChannelID: "grok-explicit-false", WireModel: "grok-4.5", APIBackend: "responses", BackendSearchSet: true},
		{ChannelID: "grok-explicit-true", WireModel: "grok-4.5", APIBackend: "responses", BackendSearchSet: true, SupportsBackendSearch: true},
	}
	effective := app.resolveSearchRoutes(context.Background(), routes, config.WebSearchSelection{})
	if len(detected) != 1 || detected[0].ChannelID != "grok-auto" {
		t.Fatalf("automatic candidates = %+v", detected)
	}
	byID := map[string]config.Route{}
	for _, route := range effective {
		byID[route.ChannelID] = route
	}
	auto := byID["grok-auto"]
	if !auto.SupportsBackendSearch || !auto.HostedSearchKnown || !auto.HostedWebSearch || auto.HostedXSearch ||
		auto.HostedChatSearchDialect != config.ChatSearchDialectWebSearchOptions {
		t.Fatalf("detected Grok route = %+v", auto)
	}
	gpt := byID["gpt-auto"]
	if gpt.SupportsBackendSearch || !gpt.HostedSearchKnown {
		t.Fatalf("missing non-Grok route changed incorrectly: %+v", gpt)
	}
	explicitFalse := byID["grok-explicit-false"]
	if explicitFalse.SupportsBackendSearch || !explicitFalse.HostedSearchKnown {
		t.Fatalf("explicit false route was not preserved: %+v", explicitFalse)
	}
	explicitTrue := byID["grok-explicit-true"]
	if !explicitTrue.SupportsBackendSearch || explicitTrue.HostedSearchKnown {
		t.Fatalf("explicit true route was not preserved: %+v", explicitTrue)
	}
}

func TestResolveSearchRoutesUnknownDetectionKeepsOfficialClientFallback(t *testing.T) {
	app := &App{
		logger:  log.New(io.Discard, "", 0),
		dataDir: t.TempDir(),
		detectSearchCapabilities: func(_ context.Context, routes []config.Route, _ string, _ bool) map[string]proxy.SearchCapabilities {
			return map[string]proxy.SearchCapabilities{
				"grok-auto": {
					WebSearch: proxy.SearchToolCapability{State: proxy.CapabilityUnknown, Source: "probe-http"},
					XSearch:   proxy.SearchToolCapability{State: proxy.CapabilityUnknown, Source: "probe-http"},
				},
			}
		},
	}
	effective := app.resolveSearchRoutes(context.Background(), []config.Route{{
		ChannelID: "grok-auto", WireModel: "grok-4.5", APIBackend: "responses",
	}}, config.WebSearchSelection{})
	if len(effective) != 1 || effective[0].SupportsBackendSearch || !effective[0].HostedSearchKnown {
		t.Fatalf("unknown capability must materialize false for Build's client fallback: %+v", effective)
	}
}
