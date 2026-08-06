package cfgpatch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestChannelProxyURLRoundTrip(t *testing.T) {
	got, err := ToChannelProxyURL("provider/model one")
	if err != nil {
		t.Fatal(err)
	}
	if got != "http://127.0.0.1:18787/c/provider%2Fmodel%20one" {
		t.Fatalf("proxy URL = %q", got)
	}
	if id := ChannelIDFromProxyURL(got); id != "provider/model one" {
		t.Fatalf("channel id = %q", id)
	}
	if IsProxyURL("https://example.test/c/model") || IsProxyURL("http://127.0.0.1:18788/c/model") {
		t.Fatal("non-proxy URL was accepted")
	}
	for _, raw := range []string{
		"http://127.0.0.2:18787/c/model",
		"http://[::1]:18787/c/model",
		"http://LOCALHOST:18787/c/model",
	} {
		if !IsProxyURL(raw) {
			t.Fatalf("loopback facade URL %q was not classified as proxy", raw)
		}
	}
}

func TestApplyTargetsAndRestoreExactConfig(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	statePath := filepath.Join(dir, "state.json")
	original := strings.Join([]string{
		"[models]",
		`default = "chat"`,
		`web_search = "user-owned-search"`,
		"stream_tool_calls = false",
		"",
		"[features] # preserve section comment",
		"web_fetch = false # user value",
		"",
		"[model.chat] # preserve section comment",
		`base_url = "https://session.example/v1" # preserve me`,
		`api_base_url = 'https://api.example/v1'`,
		`api_backend = "chat_completions"`,
		"supports_backend_search = false # user value",
		"",
		"[model.inherited]",
		`model_provider = "gateway"`,
		`model = "wire-model"`,
		"",
		"[model.official]",
		`model = "grok-4.5"`,
		"",
		"[model_providers.gateway]",
		`base_url = "https://gateway.example/v1"`,
		`api_backend = "messages"`,
		"",
	}, "\r\n")
	if err := os.WriteFile(configPath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	result, err := ApplyTargets(configPath, statePath, []Target{
		{ID: "chat", APIBaseURL: true},
		{ID: "inherited"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.BaseURLs != 2 || result.APIBaseURLs != 1 || result.APIBackends != 2 ||
		result.BackendSearch != 1 || result.BackendTools != 1 || result.WebFetch != 1 || result.ValidatedTargets != 2 {
		t.Fatalf("unexpected result: %#v", result)
	}
	patchedBytes, _ := os.ReadFile(configPath)
	patched := string(patchedBytes)
	for _, want := range []string{
		`base_url = "http://127.0.0.1:18787/c/chat"`,
		`api_base_url = "http://127.0.0.1:18787/c/chat"`,
		`base_url = "http://127.0.0.1:18787/c/inherited"`,
		`api_backend = "responses"`,
		`supports_backend_search = false # user value`,
		`backend_tools = true`,
		`web_fetch = true # user value`,
		`web_search = "user-owned-search"`,
		"stream_tool_calls = false",
		`base_url = "https://gateway.example/v1"`,
	} {
		if !strings.Contains(patched, want) {
			t.Fatalf("patched config missing %q:\n%s", want, patched)
		}
	}
	official := sectionText(t, patched, "model.official")
	if strings.Contains(official, "base_url") || strings.Contains(official, "supports_backend_search") {
		t.Fatalf("official model changed: %s", official)
	}
	inherited := sectionText(t, patched, "model.inherited")
	if !strings.Contains(inherited, "supports_backend_search = false") {
		t.Fatalf("effective missing capability was not materialized: %s", inherited)
	}

	restored, err := Restore(configPath, statePath)
	if err != nil {
		t.Fatal(err)
	}
	if restored != 9 {
		t.Fatalf("restored fields = %d", restored)
	}
	finalBytes, _ := os.ReadFile(configPath)
	if string(finalBytes) != original {
		t.Fatalf("restore was not byte-exact\nwant:\n%q\ngot:\n%q", original, string(finalBytes))
	}
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Fatalf("state file remains: %v", err)
	}
}

func TestApplyTargetsIgnoresSectionLikeTextInMultilineStrings(t *testing.T) {
	tests := []struct {
		name     string
		original string
	}{
		{
			name: "model section in multiline basic string",
			original: "[features]\nbackend_tools = false\nweb_fetch = false\n\n" +
				"[metadata]\nnotes = \"\"\"\n[model.one]\nbase_url = \"https://decoy.example/v1\"\n" +
				"api_backend = \"messages\"\nsupports_backend_search = true\n\"\"\"\n\n" +
				"[model.one]\nbase_url = \"https://real.example/v1\"\napi_backend = \"chat_completions\"\n",
		},
		{
			name: "features section in multiline literal string",
			original: "[metadata]\nnotes = '''\n[features]\nbackend_tools = false\nweb_fetch = false\n'''\n\n" +
				"[features]\nbackend_tools = false\nweb_fetch = false\n\n" +
				"[model.one]\nbase_url = \"https://real.example/v1\"\n",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			configPath := filepath.Join(dir, "config.toml")
			statePath := filepath.Join(dir, "state.json")
			if err := os.WriteFile(configPath, []byte(test.original), 0o600); err != nil {
				t.Fatal(err)
			}

			if _, err := ApplyTargets(configPath, statePath, []Target{{ID: "one"}}); err != nil {
				t.Fatal(err)
			}
			patched, err := os.ReadFile(configPath)
			if err != nil {
				t.Fatal(err)
			}
			if count := strings.Count(string(patched), `http://127.0.0.1:18787/c/one`); count != 1 {
				t.Fatalf("proxy URL count = %d, want one real model rewrite\n%s", count, patched)
			}
			if !strings.Contains(string(patched), `base_url = "https://decoy.example/v1"`) &&
				strings.Contains(test.original, "decoy.example") {
				t.Fatalf("multiline model text changed:\n%s", patched)
			}

			if _, err := Restore(configPath, statePath); err != nil {
				t.Fatal(err)
			}
			restored, err := os.ReadFile(configPath)
			if err != nil {
				t.Fatal(err)
			}
			if string(restored) != test.original {
				t.Fatalf("restore was not byte-exact\nwant: %q\ngot:  %q", test.original, restored)
			}
		})
	}
}

func TestApplyTargetsRepairsOmittedSubagentEnabledAndRestoresExactly(t *testing.T) {
	tests := []struct {
		name         string
		original     string
		wantFragment string
		wantChanged  int
	}{
		{
			name: "models child table creates parent",
			original: "[subagents.models]\n" +
				`general-purpose = "one"` + "\n\n[model.one]\nbase_url = \"https://one.example/v1\"\n",
			wantFragment: "[subagents]\nenabled = true\n[subagents.models]",
			wantChanged:  1,
		},
		{
			name:         "existing empty parent",
			original:     "[subagents]\n\n[model.one]\nbase_url = \"https://one.example/v1\"\n",
			wantFragment: "[subagents]\nenabled = true\n",
			wantChanged:  1,
		},
		{
			name:         "parent without trailing newline",
			original:     "[model.one]\nbase_url = \"https://one.example/v1\"\n\n[subagents]",
			wantFragment: "[subagents]\nenabled = true\n",
			wantChanged:  1,
		},
		{
			name: "dotted subagent config",
			original: `subagents.models.general-purpose = "one"` +
				"\n\n[model.one]\nbase_url = \"https://one.example/v1\"\n",
			wantFragment: "subagents.enabled = true\nsubagents.models.general-purpose",
			wantChanged:  1,
		},
		{
			name: "explicit true is user owned",
			original: "[subagents]\nenabled = true # explicit\n\n[subagents.models]\n" +
				`general-purpose = "one"` + "\n\n[model.one]\nbase_url = \"https://one.example/v1\"\n",
			wantFragment: "enabled = true # explicit",
			wantChanged:  0,
		},
		{
			name: "explicit false is user owned",
			original: "[subagents]\nenabled = false # explicit\n\n[subagents.models]\n" +
				`general-purpose = "one"` + "\n\n[model.one]\nbase_url = \"https://one.example/v1\"\n",
			wantFragment: "enabled = false # explicit",
			wantChanged:  0,
		},
		{
			name:         "no subagent tree",
			original:     "[model.one]\nbase_url = \"https://one.example/v1\"\n",
			wantFragment: "[model.one]",
			wantChanged:  0,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			configPath := filepath.Join(dir, "config.toml")
			statePath := filepath.Join(dir, "state.json")
			if err := os.WriteFile(configPath, []byte(test.original), 0o600); err != nil {
				t.Fatal(err)
			}

			result, err := ApplyTargets(configPath, statePath, []Target{{ID: "one"}})
			if err != nil {
				t.Fatal(err)
			}
			if result.SubagentsEnabled != test.wantChanged {
				t.Fatalf("subagent changes = %d, want %d", result.SubagentsEnabled, test.wantChanged)
			}
			patched, _ := os.ReadFile(configPath)
			if !strings.Contains(string(patched), test.wantFragment) {
				t.Fatalf("patched config missing %q:\n%s", test.wantFragment, patched)
			}

			if _, err := Restore(configPath, statePath); err != nil {
				t.Fatal(err)
			}
			restored, _ := os.ReadFile(configPath)
			if string(restored) != test.original {
				t.Fatalf("restore was not byte-exact\nwant: %q\ngot:  %q", test.original, restored)
			}
		})
	}
}

func TestApplyTargetsRepairsRealSubagentSectionNotMultilineDecoy(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	statePath := filepath.Join(dir, "state.json")
	original := "[metadata]\nnotes = \"\"\"\n[subagents]\nenabled = false\n\"\"\"\n\n" +
		"[subagents.models]\ngeneral-purpose = \"one\"\n\n" +
		"[model.one]\nbase_url = \"https://one.example/v1\"\n"
	if err := os.WriteFile(configPath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	result, err := ApplyTargets(configPath, statePath, []Target{{ID: "one"}})
	if err != nil {
		t.Fatal(err)
	}
	if result.SubagentsEnabled != 1 {
		t.Fatalf("subagent changes = %d, want 1", result.SubagentsEnabled)
	}
	patched, _ := os.ReadFile(configPath)
	if strings.Count(string(patched), "enabled = true") != 1 ||
		!strings.Contains(string(patched), "notes = \"\"\"\n[subagents]\nenabled = false") {
		t.Fatalf("multiline decoy changed or real repair missing:\n%s", patched)
	}
	if _, err := Restore(configPath, statePath); err != nil {
		t.Fatal(err)
	}
	restored, _ := os.ReadFile(configPath)
	if string(restored) != original {
		t.Fatalf("restore was not byte-exact\nwant: %q\ngot:  %q", original, restored)
	}
}

func TestApplyTargetsRejectsInvalidSubagentEnabledWithoutWriting(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	statePath := filepath.Join(dir, "state.json")
	original := "[subagents]\nenabled = \"yes\"\n\n[model.one]\nbase_url = \"https://one.example/v1\"\n"
	if err := os.WriteFile(configPath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := ApplyTargets(configPath, statePath, []Target{{ID: "one"}})
	if err == nil || !strings.Contains(err.Error(), "[subagents].enabled must be a boolean") {
		t.Fatalf("invalid subagent error = %v", err)
	}
	current, _ := os.ReadFile(configPath)
	if string(current) != original {
		t.Fatalf("config changed after validation failure: %q", current)
	}
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Fatalf("rewrite state should not exist: %v", err)
	}
}

func TestApplyTargetsRejectsInlineSubagentTableWithoutWriting(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	statePath := filepath.Join(dir, "state.json")
	original := "subagents = { models = { general-purpose = \"one\" } }\n\n" +
		"[model.one]\nbase_url = \"https://one.example/v1\"\n"
	if err := os.WriteFile(configPath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := ApplyTargets(configPath, statePath, []Target{{ID: "one"}})
	if err == nil || !strings.Contains(err.Error(), "unsupported inline subagents table") {
		t.Fatalf("inline subagent error = %v", err)
	}
	current, _ := os.ReadFile(configPath)
	if string(current) != original {
		t.Fatalf("config changed after inline-table failure: %q", current)
	}
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Fatalf("rewrite state should not exist: %v", err)
	}
}

func TestSubagentRepairCrashRecoveryAndManagedEditConflict(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	statePath := filepath.Join(dir, "state.json")
	original := "[subagents.models]\ngeneral-purpose = \"one\"\n\n" +
		"[model.one]\nbase_url = \"https://one.example/v1\"\n"
	if err := os.WriteFile(configPath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyTargets(configPath, statePath, []Target{{ID: "one"}}); err != nil {
		t.Fatal(err)
	}
	preparedState, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Restore(configPath, statePath); err != nil {
		t.Fatal(err)
	}

	// State committed before config replacement is an unapplied transaction.
	if err := os.WriteFile(statePath, preparedState, 0o600); err != nil {
		t.Fatal(err)
	}
	count, err := Restore(configPath, statePath)
	if err != nil || count != 0 {
		t.Fatalf("unapplied restore count=%d err=%v", count, err)
	}
	current, _ := os.ReadFile(configPath)
	if string(current) != original {
		t.Fatalf("unapplied transaction changed config: %q", current)
	}

	if _, err := ApplyTargets(configPath, statePath, []Target{{ID: "one"}}); err != nil {
		t.Fatal(err)
	}
	patched, _ := os.ReadFile(configPath)
	userEdited := strings.Replace(string(patched), "enabled = true", "enabled = false", 1)
	if err := os.WriteFile(configPath, []byte(userEdited), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Restore(configPath, statePath); err == nil || !strings.Contains(err.Error(), "refusing to overwrite") {
		t.Fatalf("managed subagent edit restore error = %v", err)
	}
	current, _ = os.ReadFile(configPath)
	if string(current) != userEdited {
		t.Fatalf("managed subagent edit was overwritten: %q", current)
	}
}

func TestApplyTargetsMaterializesEffectiveBackendSearchAndRestores(t *testing.T) {
	tests := []struct {
		name        string
		original    string
		target      Target
		wantLine    string
		wantChanges int
	}{
		{
			name:        "missing defaults false",
			original:    "[model.one]\nbase_url = \"https://one.example/v1\"\n",
			target:      Target{ID: "one"},
			wantLine:    "supports_backend_search = false",
			wantChanges: 1,
		},
		{
			name: "provider true is materialized on model",
			original: "[model_providers.gateway]\nbase_url = \"https://one.example/v1\"\n" +
				"supports_backend_search = true\n\n[model.one]\nmodel_provider = \"gateway\"\n",
			target:      Target{ID: "one", SupportsBackendSearch: true},
			wantLine:    "supports_backend_search = true",
			wantChanges: 1,
		},
		{
			name: "model false overrides provider true",
			original: "[model_providers.gateway]\nbase_url = \"https://one.example/v1\"\n" +
				"supports_backend_search = true\n\n[model.one]\nmodel_provider = \"gateway\"\n" +
				"supports_backend_search = false # model wins\n",
			target:      Target{ID: "one"},
			wantLine:    "supports_backend_search = false # model wins",
			wantChanges: 0,
		},
		{
			name:     "existing true and comment stay intact",
			original: "[model.one]\nbase_url = \"https://one.example/v1\"\nsupports_backend_search = true # hosted\n",
			target: Target{
				ID:                    "one",
				SupportsBackendSearch: true,
			},
			wantLine:    "supports_backend_search = true # hosted",
			wantChanges: 0,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			configPath := filepath.Join(dir, "config.toml")
			statePath := filepath.Join(dir, "state.json")
			if err := os.WriteFile(configPath, []byte(test.original), 0o600); err != nil {
				t.Fatal(err)
			}

			result, err := ApplyTargets(configPath, statePath, []Target{test.target})
			if err != nil {
				t.Fatal(err)
			}
			if result.BackendSearch != test.wantChanges {
				t.Fatalf("backend-search changes = %d, want %d", result.BackendSearch, test.wantChanges)
			}
			patched, _ := os.ReadFile(configPath)
			model := sectionText(t, string(patched), "model.one")
			if !strings.Contains(model, test.wantLine) {
				t.Fatalf("model capability missing %q:\n%s", test.wantLine, model)
			}

			if _, err := Restore(configPath, statePath); err != nil {
				t.Fatal(err)
			}
			restored, _ := os.ReadFile(configPath)
			if string(restored) != test.original {
				t.Fatalf("restore was not byte-exact\nwant: %q\ngot:  %q", test.original, restored)
			}
		})
	}
}

func TestApplyTargetsCreatesAndRemovesFeaturesSectionExactly(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	statePath := filepath.Join(dir, "state.json")
	original := "[model.one]\nbase_url = \"https://one.example/v1\""
	if err := os.WriteFile(configPath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := ApplyTargets(configPath, statePath, []Target{{ID: "one"}})
	if err != nil {
		t.Fatal(err)
	}
	if result.BackendTools != 1 || result.WebFetch != 1 {
		t.Fatalf("feature result: %#v", result)
	}
	patched, _ := os.ReadFile(configPath)
	if !strings.Contains(string(patched), "[features]\nbackend_tools = true\nweb_fetch = true\n") {
		t.Fatalf("features section missing:\n%s", patched)
	}
	if _, err := Restore(configPath, statePath); err != nil {
		t.Fatal(err)
	}
	final, _ := os.ReadFile(configPath)
	if string(final) != original {
		t.Fatalf("restore was not byte-exact\nwant: %q\ngot:  %q", original, final)
	}
}

func TestApplyTargetsLeavesNonTargetsUntouched(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	statePath := filepath.Join(dir, "state.json")
	original := "[model.one]\nbase_url = \"https://one.example/v1\"\n\n[model.two]\nbase_url = \"https://two.example/v1\"\n"
	if err := os.WriteFile(configPath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyTargets(configPath, statePath, []Target{{ID: "one"}}); err != nil {
		t.Fatal(err)
	}
	patched, _ := os.ReadFile(configPath)
	two := sectionText(t, string(patched), "model.two")
	if two != "[model.two]\nbase_url = \"https://two.example/v1\"\n" {
		t.Fatalf("non-target changed: %q", two)
	}
}

func TestApplyTargetsFailsWhenModelSectionIsMissing(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	statePath := filepath.Join(dir, "state.json")
	original := "[model.present]\nbase_url = \"https://example.test\"\n"
	if err := os.WriteFile(configPath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyTargets(configPath, statePath, []Target{{ID: "missing"}}); err == nil {
		t.Fatal("missing model section must fail")
	}
	current, _ := os.ReadFile(configPath)
	if string(current) != original {
		t.Fatal("config changed on failed apply")
	}
}

func TestApplyTargetsNormalizesWrongTypedManagedValues(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	statePath := filepath.Join(dir, "state.json")
	original := `[features]
backend_tools = "enabled" # invalid type
web_fetch = 1

[model.one]
base_url = "https://one.example/v1"
api_base_url = 7 # invalid type
api_backend = 42
supports_backend_search = false
`
	if err := os.WriteFile(configPath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	result, err := ApplyTargets(configPath, statePath, []Target{{ID: "one", APIBaseURL: true}})
	if err != nil {
		t.Fatal(err)
	}
	if result.ValidatedTargets != 1 {
		t.Fatalf("validated targets = %d", result.ValidatedTargets)
	}
	patched, _ := os.ReadFile(configPath)
	for _, want := range []string{
		"backend_tools = true",
		"web_fetch = true",
		`base_url = "http://127.0.0.1:18787/c/one"`,
		`api_base_url = "http://127.0.0.1:18787/c/one"`,
		`api_backend = "responses"`,
		"supports_backend_search = false",
	} {
		if !strings.Contains(string(patched), want) {
			t.Fatalf("normalized config missing %q:\n%s", want, patched)
		}
	}

	if _, err := Restore(configPath, statePath); err != nil {
		t.Fatal(err)
	}
	restored, _ := os.ReadFile(configPath)
	if string(restored) != original {
		t.Fatalf("restore was not byte-exact\nwant:\n%q\ngot:\n%q", original, restored)
	}
}

func TestApplyTargetsRejectsWrongTypedBackendSearchWithoutWriting(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	statePath := filepath.Join(dir, "state.json")
	original := "[model.one]\nbase_url = \"https://one.example/v1\"\nsupports_backend_search = \"yes\"\n"
	if err := os.WriteFile(configPath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := ApplyTargets(configPath, statePath, []Target{{ID: "one"}})
	if err == nil || !strings.Contains(err.Error(), "supports_backend_search must be a boolean") {
		t.Fatalf("wrong capability error = %v", err)
	}
	current, _ := os.ReadFile(configPath)
	if string(current) != original {
		t.Fatalf("config changed after capability validation failure: %q", current)
	}
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Fatalf("rewrite state should not exist: %v", err)
	}
}

func TestRestoreRejectsLegacyRewriteState(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	statePath := filepath.Join(dir, "state.json")
	patched := "[model.one]\nsupports_backend_search = true\n"
	legacyState := `{"version":3,"models":{"one":{"backend_search":{"managed":true,"present":true,"original_line":"supports_backend_search = false\n"}}}}`
	if err := os.WriteFile(configPath, []byte(patched), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath, []byte(legacyState), 0o600); err != nil {
		t.Fatal(err)
	}

	restored, err := Restore(configPath, statePath)
	if err == nil || !strings.Contains(err.Error(), "unsupported rewrite state version 3") {
		t.Fatalf("legacy state error = %v", err)
	}
	if restored != 0 {
		t.Fatalf("restored fields = %d, want 0", restored)
	}
	current, _ := os.ReadFile(configPath)
	if string(current) != patched {
		t.Fatalf("config changed after rejecting legacy state: %q", current)
	}
	if _, err := os.Stat(statePath); err != nil {
		t.Fatalf("legacy state should remain for manual recovery: %v", err)
	}
}

func TestApplyTargetsRejectsStateForDifferentConfig(t *testing.T) {
	dir := t.TempDir()
	firstPath := filepath.Join(dir, "first.toml")
	secondPath := filepath.Join(dir, "second.toml")
	statePath := filepath.Join(dir, "state.json")
	original := "[model.one]\nbase_url = \"https://one.example/v1\"\n"
	for _, path := range []string{firstPath, secondPath} {
		if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := ApplyTargets(firstPath, statePath, []Target{{ID: "one"}}); err != nil {
		t.Fatal(err)
	}

	_, err := ApplyTargets(secondPath, statePath, []Target{{ID: "one"}})
	if err == nil || !strings.Contains(err.Error(), "rewrite state belongs to") {
		t.Fatalf("wrong-config state error = %v", err)
	}
	second, _ := os.ReadFile(secondPath)
	if string(second) != original {
		t.Fatalf("unrelated config changed: %q", second)
	}
	if _, err := Restore(secondPath, statePath); err == nil || !strings.Contains(err.Error(), "rewrite state belongs to") {
		t.Fatalf("wrong-config restore error = %v", err)
	}
	if _, err := Restore(firstPath, statePath); err != nil {
		t.Fatal(err)
	}
}

func TestApplyAndRestorePreserveConfigSymlink(t *testing.T) {
	dir := t.TempDir()
	realPath := filepath.Join(dir, "real-config.toml")
	linkPath := filepath.Join(dir, "config.toml")
	statePath := filepath.Join(dir, "state.json")
	original := "[model.one]\nbase_url = \"https://one.example/v1\"\n"
	if err := os.WriteFile(realPath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realPath, linkPath); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if _, err := ApplyTargets(linkPath, statePath, []Target{{ID: "one"}}); err != nil {
		t.Fatal(err)
	}
	assertSymlink := func(stage string) {
		t.Helper()
		info, err := os.Lstat(linkPath)
		if err != nil {
			t.Fatalf("%s: lstat link: %v", stage, err)
		}
		if info.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("%s: config path is no longer a symlink", stage)
		}
	}
	assertSymlink("after apply")
	patched, _ := os.ReadFile(realPath)
	if !strings.Contains(string(patched), `base_url = "http://127.0.0.1:18787/c/one"`) {
		t.Fatalf("real config was not patched: %q", patched)
	}

	if _, err := Restore(linkPath, statePath); err != nil {
		t.Fatal(err)
	}
	assertSymlink("after restore")
	restored, _ := os.ReadFile(realPath)
	if string(restored) != original {
		t.Fatalf("restore through symlink was not byte-exact: %q", restored)
	}
}

func TestApplyTargetsRejectsInvalidPreparedTOMLWithoutWriting(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	statePath := filepath.Join(dir, "state.json")
	original := "[model.one]\nbase_url = \"https://one.example/v1\"\nbase_url = \"https://duplicate.example/v1\"\n"
	if err := os.WriteFile(configPath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := ApplyTargets(configPath, statePath, []Target{{ID: "one"}}); err == nil || !strings.Contains(err.Error(), "parse TOML") {
		t.Fatalf("invalid prepared TOML error = %v", err)
	}
	current, _ := os.ReadFile(configPath)
	if string(current) != original {
		t.Fatal("config changed after validation failure")
	}
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Fatalf("rewrite state should not exist: %v", err)
	}
}

func TestApplyTargetsIsIdempotentAndStillRestoresOriginal(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	statePath := filepath.Join(dir, "state.json")
	original := "[model.one]\nbase_url = \"https://one.example/v1\"\n"
	if err := os.WriteFile(configPath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := ApplyTargets(configPath, statePath, []Target{{ID: "one"}}); err != nil {
		t.Fatal(err)
	}
	first, _ := os.ReadFile(configPath)
	secondResult, err := ApplyTargets(configPath, statePath, []Target{{ID: "one"}})
	if err != nil {
		t.Fatal(err)
	}
	second, _ := os.ReadFile(configPath)
	if string(second) != string(first) || secondResult.ValidatedTargets != 1 {
		t.Fatalf("second apply changed config or skipped validation: %#v", secondResult)
	}
	if _, err := Restore(configPath, statePath); err != nil {
		t.Fatal(err)
	}
	restored, _ := os.ReadFile(configPath)
	if string(restored) != original {
		t.Fatalf("restore after repeated apply = %q, want %q", restored, original)
	}
}

func TestRestoreRefusesToOverwriteManagedUserEdit(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	statePath := filepath.Join(dir, "state.json")
	original := "[model.one]\nbase_url = \"https://one.example/v1\"\n"
	if err := os.WriteFile(configPath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyTargets(configPath, statePath, []Target{{ID: "one"}}); err != nil {
		t.Fatal(err)
	}
	patched, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	userEdited := strings.Replace(string(patched),
		`base_url = "http://127.0.0.1:18787/c/one"`,
		`base_url = "https://new-user-value.example/v1"`, 1)
	if err := os.WriteFile(configPath, []byte(userEdited), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := Restore(configPath, statePath); err == nil || !strings.Contains(err.Error(), "refusing to overwrite") {
		t.Fatalf("managed edit restore error = %v", err)
	}
	current, _ := os.ReadFile(configPath)
	if string(current) != userEdited {
		t.Fatalf("managed user edit was overwritten: %q", current)
	}
	if _, err := os.Stat(statePath); err != nil {
		t.Fatalf("state must remain after restore conflict: %v", err)
	}
	if _, err := ApplyTargets(configPath, statePath, []Target{{ID: "one"}}); err == nil || !strings.Contains(err.Error(), "conflicts with config") {
		t.Fatalf("reapply managed edit error = %v", err)
	}
}

func TestRestorePreservesUnrelatedConcurrentEdit(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	statePath := filepath.Join(dir, "state.json")
	original := "[model.one]\nbase_url = \"https://one.example/v1\"\n"
	if err := os.WriteFile(configPath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyTargets(configPath, statePath, []Target{{ID: "one"}}); err != nil {
		t.Fatal(err)
	}
	patched, _ := os.ReadFile(configPath)
	if err := os.WriteFile(configPath, append(patched, []byte("\n[user_edit]\nvalue = 1\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Restore(configPath, statePath); err != nil {
		t.Fatal(err)
	}
	current, _ := os.ReadFile(configPath)
	want := original + "\n[user_edit]\nvalue = 1\n"
	if string(current) != want {
		t.Fatalf("unrelated edit was not preserved\nwant: %q\ngot:  %q", want, current)
	}
}

func TestApplyTargetsRestoresSectionsWithoutTrailingNewline(t *testing.T) {
	tests := []struct {
		name     string
		original string
	}{
		{
			name:     "empty features table is last",
			original: "[model.one]\nbase_url = \"https://one.example/v1\"\n\n[features]",
		},
		{
			name: "inherited model table is last",
			original: "[features]\nbackend_tools = false\nweb_fetch = false\n\n" +
				"[model_providers.gateway]\nbase_url = \"https://one.example/v1\"\n\n[model.one]",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			configPath := filepath.Join(dir, "config.toml")
			statePath := filepath.Join(dir, "state.json")
			if err := os.WriteFile(configPath, []byte(test.original), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := ApplyTargets(configPath, statePath, []Target{{ID: "one"}}); err != nil {
				t.Fatal(err)
			}
			if _, err := Restore(configPath, statePath); err != nil {
				t.Fatal(err)
			}
			restored, _ := os.ReadFile(configPath)
			if string(restored) != test.original {
				t.Fatalf("restore was not byte-exact\nwant: %q\ngot:  %q", test.original, restored)
			}
		})
	}
}

func TestRestoreWithoutStateIsNoop(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(configPath, []byte("[models]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	count, err := Restore(configPath, filepath.Join(dir, "missing.json"))
	if err != nil || count != 0 {
		t.Fatalf("count=%d err=%v", count, err)
	}
}

func TestRestoreDiscardsStateCommittedBeforeConfigRewrite(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	statePath := filepath.Join(dir, "state.json")
	original := "[features]\nbackend_tools = false\n\n[model.one]\nbase_url = \"https://one.example/v1\"\n"
	if err := os.WriteFile(configPath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyTargets(configPath, statePath, []Target{{ID: "one"}}); err != nil {
		t.Fatal(err)
	}
	preparedState, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Restore(configPath, statePath); err != nil {
		t.Fatal(err)
	}
	withUserEdit := original + "\n[user_edit]\nvalue = 1\n"
	if err := os.WriteFile(configPath, []byte(withUserEdit), 0o600); err != nil {
		t.Fatal(err)
	}
	// Simulate a crash after the recovery record was committed but before the
	// atomic config rename. Unrelated edits must remain untouched.
	if err := os.WriteFile(statePath, preparedState, 0o600); err != nil {
		t.Fatal(err)
	}
	count, err := Restore(configPath, statePath)
	if err != nil || count != 0 {
		t.Fatalf("restore count=%d err=%v", count, err)
	}
	current, _ := os.ReadFile(configPath)
	if string(current) != withUserEdit {
		t.Fatalf("unapplied transaction changed config: %q", current)
	}
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Fatalf("unapplied state remains: %v", err)
	}
}

func TestApplyTargetsRecoversStateCommittedBeforeConfigRewrite(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	statePath := filepath.Join(dir, "state.json")
	original := "[model.one]\nbase_url = \"https://one.example/v1\"\n"
	if err := os.WriteFile(configPath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyTargets(configPath, statePath, []Target{{ID: "one"}}); err != nil {
		t.Fatal(err)
	}
	preparedState, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Restore(configPath, statePath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath, preparedState, 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := ApplyTargets(configPath, statePath, []Target{{ID: "one"}})
	if err != nil || result.ValidatedTargets != 1 {
		t.Fatalf("reapply result=%+v err=%v", result, err)
	}
	patched, _ := os.ReadFile(configPath)
	if !strings.Contains(string(patched), `base_url = "http://127.0.0.1:18787/c/one"`) {
		t.Fatalf("config was not rewritten after recovery: %s", patched)
	}
	if _, err := Restore(configPath, statePath); err != nil {
		t.Fatal(err)
	}
	restored, _ := os.ReadFile(configPath)
	if string(restored) != original {
		t.Fatalf("recovered transaction did not restore exactly: %q", restored)
	}
}

func sectionText(t *testing.T, text, name string) string {
	t.Helper()
	marker := "[" + name + "]"
	start := strings.Index(text, marker)
	if start < 0 {
		t.Fatalf("section %s missing", name)
	}
	rest := text[start+len(marker):]
	end := len(rest)
	if next := strings.Index(rest, "\n["); next >= 0 {
		end = next + 1
	}
	return text[start : start+len(marker)+end]
}
