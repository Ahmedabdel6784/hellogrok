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
		"output": []any{map[string]any{
			"type": "message",
			"content": []any{map[string]any{
				"type": "output_text",
				"text": "I used " + clientWebSearchWireAliasBase + "_2 to search.",
			}, map[string]any{
				"type": "output_text",
				"text": "Do not rewrite " + clientWebSearchWireAliasBase + "_20 because it is a different identifier.",
			}},
		}, map[string]any{
			"type":      "function_call",
			"name":      "write_file",
			"arguments": `{"content":"hellogrok_web_search","url":"https://example.test/hellogrok_web_search?q=1"}`,
		}},
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
	output := anySlice(root["output"])[0].(map[string]any)
	content := anySlice(output["content"])[0].(map[string]any)
	if got := stringValue(content["text"]); got != "I used "+alias+" to search." {
		t.Fatalf("user-visible text was rewritten: %q", got)
	}
	longerIdentifier := anySlice(output["content"])[1].(map[string]any)
	if got := stringValue(longerIdentifier["text"]); got != "Do not rewrite "+clientWebSearchWireAliasBase+"_20 because it is a different identifier." {
		t.Fatalf("longer identifier was corrupted: %q", got)
	}
	unrelatedCall := anySlice(root["output"])[1].(map[string]any)
	wantArguments := `{"content":"hellogrok_web_search","url":"https://example.test/hellogrok_web_search?q=1"}`
	if got := stringValue(unrelatedCall["arguments"]); got != wantArguments {
		t.Fatalf("unrelated tool arguments were rewritten: %q", got)
	}
}
