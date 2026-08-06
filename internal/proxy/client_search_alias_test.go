package proxy

import "testing"

func TestClientSearchWireAliasAvoidsCollisionsAndLeavesHostedToolsUntouched(t *testing.T) {
	root := map[string]any{
		"tools": []any{
			map[string]any{"type": "function", "name": "web_search"},
			map[string]any{"type": "function", "name": clientWebSearchWireAliasBase},
			map[string]any{"type": "web_search", "name": "web_search"},
		},
		"tool_choice": map[string]any{"type": "function", "name": "web_search"},
	}
	alias := chooseClientWebSearchWireAlias(root)
	if alias != clientWebSearchWireAliasBase+"_2" {
		t.Fatalf("collision-safe alias=%q", alias)
	}
	if !aliasClientWebSearchOnWire(root, alias) {
		t.Fatal("client search was not aliased")
	}
	tools := anySlice(root["tools"])
	client, _ := tools[0].(map[string]any)
	hosted, _ := tools[2].(map[string]any)
	choice, _ := root["tool_choice"].(map[string]any)
	if stringValue(client["name"]) != alias || stringValue(choice["name"]) != alias {
		t.Fatalf("client names were not aliased: %#v %#v", client, choice)
	}
	if stringValue(hosted["name"]) != "web_search" {
		t.Fatalf("hosted tool was rewritten: %#v", hosted)
	}
	if !restoreClientWebSearchAlias(root, alias) || stringValue(client["name"]) != "web_search" ||
		stringValue(choice["name"]) != "web_search" {
		t.Fatalf("client alias was not restored: %#v", root)
	}
}
