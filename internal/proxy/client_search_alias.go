package proxy

import (
	"encoding/json"
	"fmt"
	"strings"
)

const clientWebSearchWireAliasBase = "hellogrok_web_search"

// chooseClientWebSearchWireAlias keeps Build's public web_search name away
// from upstreams that reserve or intercept that name as a hosted tool.
func chooseClientWebSearchWireAlias(root map[string]any) string {
	used := map[string]struct{}{}
	hasClientSearch := false
	for _, raw := range anySlice(root["tools"]) {
		tool, _ := raw.(map[string]any)
		name := strings.ToLower(strings.TrimSpace(functionToolName(tool)))
		if name == "" {
			continue
		}
		used[name] = struct{}{}
		if name == "web_search" {
			hasClientSearch = true
		}
	}
	if !hasClientSearch {
		return ""
	}
	for suffix := 0; ; suffix++ {
		candidate := clientWebSearchWireAliasBase
		if suffix > 0 {
			candidate = fmt.Sprintf("%s_%d", clientWebSearchWireAliasBase, suffix+1)
		}
		if _, exists := used[candidate]; !exists {
			return candidate
		}
	}
}

// aliasClientWebSearchOnWire rewrites only function-shaped names. Hosted
// server_tool_use/web_search declarations must retain their provider schema.
func aliasClientWebSearchOnWire(value any, alias string) bool {
	if strings.TrimSpace(alias) == "" {
		return false
	}
	changed := false
	var walk func(any)
	walk = func(current any) {
		switch typed := current.(type) {
		case map[string]any:
			typ := strings.ToLower(strings.TrimSpace(stringValue(typed["type"])))
			_, messagesFunction := typed["input_schema"]
			if typ == "function" || typ == "function_call" || typ == "tool_use" ||
				typ == "tool" || messagesFunction {
				if strings.EqualFold(strings.TrimSpace(stringValue(typed["name"])), "web_search") {
					typed["name"] = alias
					changed = true
				}
			}
			if typ == "function" {
				if function, _ := typed["function"].(map[string]any); function != nil &&
					strings.EqualFold(strings.TrimSpace(stringValue(function["name"])), "web_search") {
					function["name"] = alias
					changed = true
				}
			}
			for _, child := range typed {
				walk(child)
			}
		case []any:
			for _, child := range typed {
				walk(child)
			}
		}
	}
	walk(value)
	return changed
}

func restoreClientWebSearchAlias(value any, alias string) bool {
	if strings.TrimSpace(alias) == "" {
		return false
	}
	changed := false
	var walk func(any)
	walk = func(current any) {
		switch typed := current.(type) {
		case map[string]any:
			if strings.EqualFold(strings.TrimSpace(stringValue(typed["name"])), alias) {
				typed["name"] = "web_search"
				changed = true
			}
			for key, child := range typed {
				if text, ok := child.(string); ok {
					if restored, restoredText := replaceIdentifier(text, alias, "web_search"); restoredText {
						typed[key] = restored
						changed = true
					}
					continue
				}
				walk(child)
			}
		case []any:
			for index, child := range typed {
				if text, ok := child.(string); ok {
					if restored, restoredText := replaceIdentifier(text, alias, "web_search"); restoredText {
						typed[index] = restored
						changed = true
					}
					continue
				}
				walk(child)
			}
		}
	}
	walk(value)
	return changed
}

func replaceIdentifier(value, old, replacement string) (string, bool) {
	searchFrom := 0
	writtenThrough := 0
	changed := false
	var restored strings.Builder
	for searchFrom < len(value) {
		relative := strings.Index(value[searchFrom:], old)
		if relative < 0 {
			break
		}
		start := searchFrom + relative
		end := start + len(old)
		if (start > 0 && identifierByte(value[start-1])) || (end < len(value) && identifierByte(value[end])) {
			searchFrom = end
			continue
		}
		if !changed {
			restored.Grow(len(value))
		}
		restored.WriteString(value[writtenThrough:start])
		restored.WriteString(replacement)
		writtenThrough = end
		searchFrom = end
		changed = true
	}
	if !changed {
		return value, false
	}
	restored.WriteString(value[writtenThrough:])
	return restored.String(), true
}

func identifierByte(value byte) bool {
	return value == '_' || value >= '0' && value <= '9' || value >= 'A' && value <= 'Z' || value >= 'a' && value <= 'z'
}

func restoreClientWebSearchAliasJSON(data []byte, alias string) ([]byte, error) {
	root, err := decodeJSONMap(data)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(alias) == "" || !restoreClientWebSearchAlias(root, alias) {
		return data, nil
	}
	encoded, err := json.Marshal(root)
	if err != nil {
		return nil, err
	}
	return encoded, nil
}

func isClientWebSearchWireAlias(name string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	return name == clientWebSearchWireAliasBase || strings.HasPrefix(name, clientWebSearchWireAliasBase+"_")
}
