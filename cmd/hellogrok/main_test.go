package main

import (
	"bytes"
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
	if enabled, err := first.ProxyEnabledOnLaunch(); err != nil || enabled {
		t.Fatalf("initial enabled=%v err=%v", enabled, err)
	}
	if err := first.SetProxyEnabledOnLaunch(true); err != nil {
		t.Fatal(err)
	}
	second := &App{dataDir: dataDir}
	if enabled, err := second.ProxyEnabledOnLaunch(); err != nil || !enabled {
		t.Fatalf("remembered enabled=%v err=%v", enabled, err)
	}
	if err := second.SetProxyEnabledOnLaunch(false); err != nil {
		t.Fatal(err)
	}
	if enabled, err := first.ProxyEnabledOnLaunch(); err != nil || enabled {
		t.Fatalf("remembered disabled=%v err=%v", enabled, err)
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
