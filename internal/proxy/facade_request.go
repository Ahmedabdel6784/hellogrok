package proxy

import (
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"strings"

	"github.com/hellowind777/hellogrok/internal/config"
)

type facadeRequest struct {
	Body                 []byte
	Protocol             wireProtocol
	Stream               bool
	HostedWebSearch      bool
	SearchQuery          string
	BuildHostedWebSearch int
	BuildXSearch         int
	ProxyAddedWebSearch  bool
	ClientSearchPrepared bool
	ClientSearchAlias    string
	ReplayScope          string
}

func channelFromPath(escapedPath string) (string, bool) {
	parts := strings.Split(strings.Trim(escapedPath, "/"), "/")
	if len(parts) != 3 || parts[0] != "c" || parts[2] != "responses" {
		return "", false
	}
	id, err := url.PathUnescape(parts[1])
	if err != nil || strings.TrimSpace(id) == "" {
		return "", false
	}
	return id, true
}

func upstreamTarget(route config.Route, rawQuery string) (string, wireProtocol, error) {
	u, err := url.Parse(route.OriginBase)
	if err != nil || u.Host == "" {
		return "", wireUnknown, fmt.Errorf("invalid upstream base_url")
	}
	var suffix string
	var protocol wireProtocol
	switch strings.ToLower(strings.TrimSpace(route.APIBackend)) {
	case "responses":
		suffix, protocol = "/responses", wireResponses
	case "messages":
		suffix, protocol = "/messages", wireMessages
	case "chat_completions", "":
		suffix, protocol = "/chat/completions", wireChatCompletions
	default:
		return "", wireUnknown, fmt.Errorf("unsupported api_backend %q", route.APIBackend)
	}
	u.Path = strings.TrimRight(u.Path, "/") + suffix
	u.RawPath = ""
	if rawQuery != "" {
		incoming, err := url.ParseQuery(rawQuery)
		if err != nil {
			return "", wireUnknown, fmt.Errorf("invalid request query parameters: %w", err)
		}
		merged := u.Query()
		for key, values := range incoming {
			merged.Del(key)
			for _, value := range values {
				merged.Add(key, value)
			}
		}
		u.RawQuery = merged.Encode()
	}
	return u.String(), protocol, nil
}

func adaptFacadeRequest(body []byte, route config.Route, replays *searchReplayCache) (facadeRequest, error) {
	root, err := decodeRequestObject(body)
	if err != nil {
		return facadeRequest{}, fmt.Errorf("decode Responses request: %w", err)
	}
	stream, _ := root["stream"].(bool)
	replayScope := replayConversationFingerprint(root["input"])
	_, _, buildHostedSearch, _, buildXSearch := summarizeBody(body)
	root["model"] = route.WireModel
	clientSearchPrepared := prepareClientSearchExecution(root, buildHostedSearch, buildXSearch)
	proxyAddedSearch := false
	clientSearchAlias := ""
	capabilities := hostedSearchCapabilities{}
	if clientSearchPrepared {
		// Build's WebSearchClient always sends exactly one hosted web_search
		// request. It is independent of the conversation model's backend-search
		// setting and must never gain x_search.
		capabilities.Web = true
	} else if route.SupportsBackendSearch {
		capabilities = routeHostedSearchCapabilities(route)
		proxyAddedSearch = ensureHostedSearch(root, capabilities)
	} else {
		describeClientWebTools(root)
		clientSearchAlias = chooseClientWebSearchWireAlias(root)
	}
	normalizeHostedSearchObject(root, capabilities)
	hosted := hasHostedSearchTool(root)
	query := lastUserText(root["input"])
	requestInfo := facadeRequest{
		Stream:               stream,
		HostedWebSearch:      hosted,
		SearchQuery:          query,
		BuildHostedWebSearch: buildHostedSearch,
		BuildXSearch:         buildXSearch,
		ProxyAddedWebSearch:  proxyAddedSearch,
		ClientSearchPrepared: clientSearchPrepared,
		ClientSearchAlias:    clientSearchAlias,
		ReplayScope:          replayScope,
	}

	switch strings.ToLower(strings.TrimSpace(route.APIBackend)) {
	case "responses":
		replays.restore(route.ChannelID, root)
		if !aliasClientWebSearchOnWire(root, clientSearchAlias) {
			requestInfo.ClientSearchAlias = ""
		}
		encoded, err := encodeRequestObject(root)
		if err != nil {
			return facadeRequest{}, err
		}
		requestInfo.Body = encoded
		requestInfo.Protocol = wireResponses
		return requestInfo, nil
	case "messages":
		converted, err := responsesToMessagesRequest(root, route.ChannelID, replays)
		if err != nil {
			return facadeRequest{}, err
		}
		if !aliasClientWebSearchOnWire(converted, clientSearchAlias) {
			requestInfo.ClientSearchAlias = ""
		}
		encoded, err := json.Marshal(converted)
		requestInfo.Body = encoded
		requestInfo.Protocol = wireMessages
		return requestInfo, err
	case "chat_completions", "":
		converted, err := responsesToChatRequest(root, route)
		if err != nil {
			return facadeRequest{}, err
		}
		if !aliasClientWebSearchOnWire(converted, clientSearchAlias) {
			requestInfo.ClientSearchAlias = ""
		}
		encoded, err := json.Marshal(converted)
		requestInfo.Body = encoded
		requestInfo.Protocol = wireChatCompletions
		return requestInfo, err
	default:
		return facadeRequest{}, fmt.Errorf("unsupported api_backend %q", route.APIBackend)
	}
}

const clientWebSearchDescription = "Search the public web for information, sources, current facts, or URLs. Use this tool whenever the user asks to search, browse, look up, verify, or obtain up-to-date information. This invokes Grok Build's configured client web-search model. In all user-visible text, refer to this tool only as web_search and never mention its internal wire name. Do not use web_fetch as a substitute for web search."

const clientWebFetchDescription = "Fetch and read one specific URL that is already known. Do not use this tool to search, discover URLs, or fetch a search-engine results page; use web_search first for those tasks."

const clientSearchExecutionInstructions = "Execute the hosted web_search for the supplied query. Use no more than four search calls, then always return a concise final text synthesis. Include the relevant source titles and URLs in that final text. Never finish with only reasoning or tool-call items."

// describeClientWebTools makes Build's two client-side web tools unambiguous
// to third-party models. The caller's structured tool choice remains the only
// source of mandatory selection; otherwise the conversation model decides.
func describeClientWebTools(root map[string]any) {
	if root == nil {
		return
	}
	for _, raw := range anySlice(root["tools"]) {
		tool, _ := raw.(map[string]any)
		if tool == nil || stringValue(tool["type"]) != "function" {
			continue
		}
		name := strings.ToLower(strings.TrimSpace(functionToolName(tool)))
		switch name {
		case "web_search":
			setFunctionToolDescription(tool, clientWebSearchDescription)
		case "web_fetch":
			setFunctionToolDescription(tool, clientWebFetchDescription)
		}
	}
}

// prepareClientSearchExecution recognizes the small, non-streaming hosted
// request emitted by Build's WebSearchClient after the main model calls the
// client web_search function. Requiring a final synthesis prevents agentic
// search providers from exhausting the turn with search/reasoning items and no
// OutputText, which Build otherwise reports as "No search results found."
func prepareClientSearchExecution(root map[string]any, buildHostedSearch, buildXSearch int) bool {
	if root == nil || buildHostedSearch != 1 || buildXSearch != 0 || toolChoiceDisablesTools(root["tool_choice"]) {
		return false
	}
	if stream, _ := root["stream"].(bool); stream {
		return false
	}
	store, hasStore := root["store"].(bool)
	query, hasStringInput := root["input"].(string)
	if !hasStore || store || !hasStringInput || strings.TrimSpace(query) == "" {
		return false
	}
	tools := anySlice(root["tools"])
	if len(tools) != 1 || !numberEquals(root["temperature"], 0.1) ||
		!numberEquals(root["top_p"], 0.95) || numberInt(root["max_output_tokens"]) != 8192 {
		return false
	}
	for _, raw := range tools {
		tool, _ := raw.(map[string]any)
		typ := strings.ToLower(strings.TrimSpace(stringValue(tool["type"])))
		if typ != "x_search" && !isHostedWebSearchType(typ) {
			return false
		}
	}
	if instructions := strings.TrimSpace(stringValue(root["instructions"])); instructions == "" {
		root["instructions"] = clientSearchExecutionInstructions
	} else if !strings.Contains(instructions, clientSearchExecutionInstructions) {
		root["instructions"] = instructions + "\n\n" + clientSearchExecutionInstructions
	}
	if automaticToolChoice(root["tool_choice"]) {
		root["tool_choice"] = map[string]any{"type": "web_search"}
	}
	return true
}

func numberEquals(value any, expected float64) bool {
	var actual float64
	switch number := value.(type) {
	case json.Number:
		parsed, err := number.Float64()
		if err != nil {
			return false
		}
		actual = parsed
	case float64:
		actual = number
	case float32:
		actual = float64(number)
	default:
		return false
	}
	return actual == expected
}

func setFunctionToolDescription(tool map[string]any, description string) {
	if function, _ := tool["function"].(map[string]any); function != nil {
		function["description"] = description
		return
	}
	tool["description"] = description
}

func automaticToolChoice(choice any) bool {
	if choice == nil {
		return true
	}
	if value, ok := choice.(string); ok {
		value = strings.ToLower(strings.TrimSpace(value))
		return value == "" || value == "auto" || value == "required"
	}
	value, ok := choice.(map[string]any)
	if !ok || strings.ToLower(strings.TrimSpace(stringValue(value["type"]))) != "allowed_tools" {
		return false
	}
	mode := strings.ToLower(strings.TrimSpace(stringValue(value["mode"])))
	return mode == "" || mode == "auto" || mode == "required"
}

func allowedToolNames(choice any) (map[string]struct{}, string, bool) {
	value, ok := choice.(map[string]any)
	if !ok || !strings.EqualFold(strings.TrimSpace(stringValue(value["type"])), "allowed_tools") {
		return nil, "", false
	}
	mode := strings.ToLower(strings.TrimSpace(stringValue(value["mode"])))
	if mode == "" {
		mode = "auto"
	}
	allowed := make(map[string]struct{})
	for _, raw := range anySlice(value["tools"]) {
		tool, _ := raw.(map[string]any)
		if tool == nil {
			continue
		}
		typ := strings.ToLower(strings.TrimSpace(stringValue(tool["type"])))
		if typ == "function" {
			if name := strings.ToLower(strings.TrimSpace(functionToolName(tool))); name != "" {
				allowed["function:"+name] = struct{}{}
			}
			continue
		}
		if typ == "x_search" || isHostedWebSearchType(typ) {
			allowed["hosted:web_search"] = struct{}{}
		}
	}
	return allowed, mode, true
}

func toolChoiceAllowsFunction(choice any, name string) bool {
	allowed, _, constrained := allowedToolNames(choice)
	if !constrained {
		return true
	}
	_, ok := allowed["function:"+strings.ToLower(strings.TrimSpace(name))]
	return ok
}

func toolChoiceAllowsHostedSearch(choice any) bool {
	allowed, _, constrained := allowedToolNames(choice)
	if !constrained {
		return true
	}
	if _, ok := allowed["hosted:web_search"]; ok {
		return true
	}
	_, webSearch := allowed["function:web_search"]
	_, xSearch := allowed["function:x_search"]
	return webSearch || xSearch
}

func routeHostedSearchCapabilities(route config.Route) hostedSearchCapabilities {
	return hostedSearchCapabilities{
		Web: true,
		X:   strings.EqualFold(strings.TrimSpace(route.APIBackend), "responses") && isGrokRoute(route),
	}
}

// ensureHostedSearch exposes hosted tools only for channels whose effective
// route enables backend search. Capability-aware normalization below removes
// any unsupported member of the pair.
func ensureHostedSearch(root map[string]any, capabilities hostedSearchCapabilities) bool {
	if root == nil || toolChoiceDisablesTools(root["tool_choice"]) ||
		!toolChoiceAllowsHostedSearch(root["tool_choice"]) || hasHostedSearchTool(root) ||
		!capabilities.any() {
		return false
	}
	tools := anySlice(root["tools"])
	webAdded := false
	if capabilities.Web {
		tools = append(tools, map[string]any{"type": "web_search"})
		webAdded = true
	}
	if capabilities.X {
		tools = append(tools, map[string]any{"type": "x_search"})
	}
	root["tools"] = tools
	return webAdded
}

func hasHostedSearchTool(root map[string]any) bool {
	tools, _ := root["tools"].([]any)
	for _, raw := range tools {
		tool, _ := raw.(map[string]any)
		typ, _ := tool["type"].(string)
		if typ == "x_search" || isHostedWebSearchType(typ) {
			return true
		}
	}
	return false
}

func responsesToMessagesRequest(root map[string]any, channel string, replays *searchReplayCache) (map[string]any, error) {
	messages, system, err := responsesInputToMessages(root["input"], true, channel, replays)
	if err != nil {
		return nil, err
	}
	if instructions := stringValue(root["instructions"]); instructions != "" {
		system = append([]string{instructions}, system...)
	}
	out := map[string]any{
		"model":      stringValue(root["model"]),
		"messages":   messages,
		"max_tokens": positiveInt(root["max_output_tokens"], 8192),
		"stream":     root["stream"] == true,
	}
	if len(system) > 0 {
		out["system"] = strings.Join(system, "\n\n")
	}
	copyIfPresent(out, root, "temperature", "top_p")

	if !toolChoiceDisablesTools(root["tool_choice"]) {
		tools, hosted := messagesTools(root["tools"], root["tool_choice"])
		if len(tools) > 0 {
			out["tools"] = tools
			if choice := messagesToolChoice(root["tool_choice"], hosted); choice != nil {
				out["tool_choice"] = choice
			}
		}
	}
	if reasoning, _ := root["reasoning"].(map[string]any); reasoning != nil {
		if effort := stringValue(reasoning["effort"]); effort != "" {
			out["thinking"] = map[string]any{"type": "adaptive", "display": "summarized"}
			out["output_config"] = map[string]any{"effort": effort}
		}
	}
	if text, _ := root["text"].(map[string]any); text != nil {
		if format, _ := text["format"].(map[string]any); format != nil && stringValue(format["type"]) == "json_schema" {
			schema := format["schema"]
			if schema == nil {
				if js, _ := format["json_schema"].(map[string]any); js != nil {
					schema = js["schema"]
				}
			}
			if schema != nil {
				cfg, _ := out["output_config"].(map[string]any)
				if cfg == nil {
					cfg = map[string]any{}
					out["output_config"] = cfg
				}
				cfg["format"] = map[string]any{"type": "json_schema", "schema": schema}
			}
		}
	}
	return out, nil
}

func responsesToChatRequest(root map[string]any, route config.Route) (map[string]any, error) {
	messages, _, err := responsesInputToMessages(root["input"], false, "", nil)
	if err != nil {
		return nil, err
	}
	if instructions := stringValue(root["instructions"]); instructions != "" {
		messages = append([]any{map[string]any{"role": "system", "content": instructions}}, messages...)
	}
	out := map[string]any{
		"model":    stringValue(root["model"]),
		"messages": messages,
		"stream":   root["stream"] == true,
	}
	if root["stream"] == true {
		// OpenAI-compatible gateways report final token usage in a trailing
		// streaming chunk only when this option is enabled.
		out["stream_options"] = map[string]any{"include_usage": true}
	}
	if n := positiveInt(root["max_output_tokens"], 0); n > 0 {
		out["max_tokens"] = n
	}
	copyIfPresent(out, root, "temperature", "top_p")

	var tools []any
	hosted := hasHostedSearchTool(root) && toolChoiceAllowsHostedSearch(root["tool_choice"])
	toolsDisabled := toolChoiceDisablesTools(root["tool_choice"])
	for _, raw := range anySlice(root["tools"]) {
		if toolsDisabled {
			break
		}
		tool, _ := raw.(map[string]any)
		typ := stringValue(tool["type"])
		if typ != "function" || !toolChoiceAllowsFunction(root["tool_choice"], functionToolName(tool)) ||
			(hosted && isSearchFunctionTool(tool)) {
			continue
		}
		fn := map[string]any{"name": tool["name"], "parameters": valueOr(tool["parameters"], map[string]any{"type": "object"})}
		if desc := stringValue(tool["description"]); desc != "" {
			fn["description"] = desc
		}
		if strict, ok := tool["strict"]; ok {
			fn["strict"] = strict
		}
		tools = append(tools, map[string]any{"type": "function", "function": fn})
	}
	if len(tools) > 0 {
		out["tools"] = tools
	}
	if !toolsDisabled && len(tools) > 0 {
		if choice := chatToolChoice(root["tool_choice"], hosted); choice != nil {
			out["tool_choice"] = choice
		}
	}
	if hosted && !toolsDisabled {
		switch chatSearchDialect(route) {
		case config.ChatSearchDialectSearchParameters:
			mode := "auto"
			if hostedToolChoiceRequired(root["tool_choice"]) {
				mode = "on"
			}
			out["search_parameters"] = map[string]any{"mode": mode, "sources": []any{map[string]any{"type": "web"}}}
		default:
			out["web_search_options"] = map[string]any{}
		}
	}
	if reasoning, _ := root["reasoning"].(map[string]any); reasoning != nil {
		if effort := stringValue(reasoning["effort"]); effort != "" {
			out["reasoning_effort"] = effort
		}
	}
	return out, nil
}

func responsesInputToMessages(input any, anthropic bool, channel string, replays *searchReplayCache) ([]any, []string, error) {
	var messages []any
	var system []string
	var pendingReasoning []string
	chatAssistantIndex := -1
	if text, ok := input.(string); ok {
		return []any{map[string]any{"role": "user", "content": text}}, nil, nil
	}
	items := anySlice(input)
	for index, raw := range items {
		item, _ := raw.(map[string]any)
		if item == nil {
			continue
		}
		typ := stringValue(item["type"])
		role := stringValue(item["role"])
		if typ == "" && role != "" {
			typ = "message"
		}
		switch typ {
		case "message":
			content := convertMessageContent(item["content"], anthropic)
			if role == "system" && anthropic {
				if text := contentText(content); text != "" {
					system = append(system, text)
				}
				continue
			}
			if anthropic && role == "assistant" {
				appendAnthropicAssistantContent(&messages, content)
			} else if !anthropic && role == "assistant" {
				appendChatAssistant(&messages, content, &pendingReasoning)
				chatAssistantIndex = len(messages) - 1
			} else {
				pendingReasoning = nil
				chatAssistantIndex = -1
				appendMessage(&messages, role, content)
			}
		case "function_call":
			id := firstString(item, "call_id", "id")
			name := stringValue(item["name"])
			arguments := stringValue(item["arguments"])
			if anthropic {
				var input any = map[string]any{}
				if arguments != "" {
					decoded, err := decodeRequestObject([]byte(arguments))
					if err != nil {
						return nil, nil, fmt.Errorf("function call %q arguments must be one JSON object: %w", name, err)
					}
					input = decoded
				}
				appendBlockToRole(&messages, "assistant", map[string]any{"type": "tool_use", "id": id, "name": name, "input": input})
			} else {
				appendChatToolCall(&messages, map[string]any{"id": id, "type": "function", "function": map[string]any{"name": name, "arguments": arguments}}, &pendingReasoning, &chatAssistantIndex)
			}
		case "function_call_output":
			id := firstString(item, "call_id", "id")
			output := responseOutputContent(item["output"], anthropic)
			pendingReasoning = nil
			chatAssistantIndex = -1
			if anthropic {
				appendMessage(&messages, "user", []any{map[string]any{"type": "tool_result", "tool_use_id": id, "content": output}})
			} else {
				messages = append(messages, map[string]any{"role": "tool", "tool_call_id": id, "content": output})
			}
		case "reasoning":
			text := reasoningInputText(item)
			if anthropic {
				signature := stringValue(item["encrypted_content"])
				if text != "" || signature != "" {
					appendBlockToRole(&messages, "assistant", map[string]any{"type": "thinking", "thinking": text, "signature": signature})
				}
			} else if text != "" {
				pendingReasoning = append(pendingReasoning, text)
				chatAssistantIndex = -1
			}
		case "web_search_call":
			if anthropic {
				conversation := replayConversationFingerprint(items[:index])
				for _, block := range replays.messagesBlocks(channel, conversation, item) {
					appendBlockToRole(&messages, "assistant", block)
				}
				continue
			}
			summary := backendToolSummary(item)
			if summary == "" {
				continue
			}
			appendMessage(&messages, "assistant", summary)
			chatAssistantIndex = -1
		case "custom_tool_call", "code_interpreter_call":
			summary := backendToolSummary(item)
			if summary == "" {
				continue
			}
			if anthropic {
				appendBlockToRole(&messages, "assistant", map[string]any{"type": "text", "text": summary})
			} else {
				appendMessage(&messages, "assistant", summary)
				chatAssistantIndex = -1
			}
		}
	}
	return messages, system, nil
}

func appendAnthropicAssistantContent(messages *[]any, content any) {
	if text, ok := content.(string); ok {
		if text != "" {
			appendBlockToRole(messages, "assistant", map[string]any{"type": "text", "text": text})
		}
		return
	}
	for _, raw := range anySlice(content) {
		if block, ok := raw.(map[string]any); ok {
			appendBlockToRole(messages, "assistant", block)
		}
	}
}

func appendChatAssistant(messages *[]any, content any, pendingReasoning *[]string) {
	message := map[string]any{"role": "assistant", "content": content}
	if len(*pendingReasoning) > 0 {
		message["reasoning_content"] = strings.Join(*pendingReasoning, "\n")
		*pendingReasoning = nil
	}
	*messages = append(*messages, message)
}

func reasoningInputText(item map[string]any) string {
	var parts []string
	for _, field := range []string{"summary", "content"} {
		for _, raw := range anySlice(item[field]) {
			part, _ := raw.(map[string]any)
			if text := stringValue(part["text"]); text != "" {
				parts = append(parts, text)
			}
		}
	}
	return strings.Join(parts, "\n")
}

func backendToolSummary(item map[string]any) string {
	switch stringValue(item["type"]) {
	case "web_search_call":
		action, _ := item["action"].(map[string]any)
		switch stringValue(action["type"]) {
		case "search", "":
			query := stringValue(action["query"])
			if query == "" {
				query = firstSearchQuery(action["queries"])
			}
			return "[backend web_search] search: " + query
		case "open_page", "open":
			url := stringValue(action["url"])
			if url == "" {
				url = "?"
			}
			return "[backend web_search] open: " + url
		case "find", "find_in_page":
			return fmt.Sprintf("[backend web_search] find %q in %s", stringValue(action["pattern"]), stringValue(action["url"]))
		}
	case "custom_tool_call":
		return fmt.Sprintf("[backend x_search] %s(%s)", stringValue(item["name"]), stringValue(item["input"]))
	case "code_interpreter_call":
		code := stringValue(item["code"])
		if len(code) > 100 {
			code = code[:100] + "..."
		}
		return "[backend code_interpreter] " + code
	}
	return ""
}

func firstSearchQuery(value any) string {
	for _, raw := range anySlice(value) {
		if query := strings.TrimSpace(stringValue(raw)); query != "" && !strings.HasPrefix(query, "ws_call_id=") {
			return query
		}
	}
	return ""
}

func convertMessageContent(value any, anthropic bool) any {
	if text, ok := value.(string); ok {
		return text
	}
	var blocks []any
	for _, raw := range anySlice(value) {
		part, _ := raw.(map[string]any)
		if part == nil {
			continue
		}
		typ := stringValue(part["type"])
		switch typ {
		case "input_text", "output_text", "text":
			blocks = append(blocks, map[string]any{"type": "text", "text": stringValue(part["text"])})
		case "input_image", "image_url":
			imageURL := stringValue(part["image_url"])
			if imageMap, _ := part["image_url"].(map[string]any); imageURL == "" && imageMap != nil {
				imageURL = stringValue(imageMap["url"])
			}
			if anthropic {
				blocks = append(blocks, anthropicImageBlock(imageURL))
			} else {
				blocks = append(blocks, map[string]any{"type": "image_url", "image_url": map[string]any{"url": imageURL}})
			}
		}
	}
	if len(blocks) == 1 {
		if block, _ := blocks[0].(map[string]any); block != nil && stringValue(block["type"]) == "text" {
			return stringValue(block["text"])
		}
	}
	return blocks
}

func anthropicImageBlock(raw string) map[string]any {
	if strings.HasPrefix(raw, "data:") {
		header, data, ok := strings.Cut(raw, ",")
		if ok {
			media := strings.TrimSuffix(strings.TrimPrefix(header, "data:"), ";base64")
			return map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": media, "data": data}}
		}
	}
	return map[string]any{"type": "image", "source": map[string]any{"type": "url", "url": raw}}
}

func messagesTools(value, choice any) ([]any, bool) {
	var tools []any
	hosted := false
	for _, raw := range anySlice(value) {
		tool, _ := raw.(map[string]any)
		typ := stringValue(tool["type"])
		if (typ == "x_search" || isHostedWebSearchType(typ)) && toolChoiceAllowsHostedSearch(choice) {
			hosted = true
			break
		}
	}
	for _, raw := range anySlice(value) {
		tool, _ := raw.(map[string]any)
		typ := stringValue(tool["type"])
		switch {
		case typ == "function":
			if !toolChoiceAllowsFunction(choice, functionToolName(tool)) {
				continue
			}
			if hosted && isSearchFunctionTool(tool) {
				continue
			}
			entry := map[string]any{"name": tool["name"], "input_schema": valueOr(tool["parameters"], map[string]any{"type": "object"})}
			if desc := stringValue(tool["description"]); desc != "" {
				entry["description"] = desc
			}
			tools = append(tools, entry)
		case (typ == "x_search" || isHostedWebSearchType(typ)) && hosted:
			if !containsMessagesHostedSearch(tools) {
				tools = append(tools, map[string]any{"type": "web_search_20250305", "name": "web_search", "max_uses": 10})
			}
		}
	}
	return tools, hosted
}

func containsMessagesHostedSearch(tools []any) bool {
	for _, raw := range tools {
		tool, _ := raw.(map[string]any)
		if stringValue(tool["name"]) == "web_search" && isHostedWebSearchType(stringValue(tool["type"])) {
			return true
		}
	}
	return false
}

func isXAIChatRoute(route config.Route) bool {
	return isGrokRoute(route)
}

func chatSearchDialect(route config.Route) config.ChatSearchDialect {
	if isXAIChatRoute(route) {
		return config.ChatSearchDialectSearchParameters
	}
	return config.ChatSearchDialectWebSearchOptions
}

func isGrokRoute(route config.Route) bool {
	model := strings.ToLower(strings.TrimSpace(route.WireModel))
	channel := strings.ToLower(strings.TrimSpace(route.ChannelID))
	host := strings.ToLower(strings.TrimSpace(route.Host))
	if parsedHost, _, err := net.SplitHostPort(host); err == nil {
		host = parsedHost
	}
	host = strings.Trim(host, "[]")
	return grokIdentifier(model) || grokIdentifier(channel) ||
		host == "x.ai" || strings.HasSuffix(host, ".x.ai")
}

func grokIdentifier(value string) bool {
	value = strings.TrimSpace(strings.ToLower(value))
	if slash := strings.LastIndex(value, "/"); slash >= 0 {
		value = value[slash+1:]
	}
	return value == "grok" || strings.HasPrefix(value, "grok-") || strings.HasPrefix(value, "grok_")
}

func messagesToolChoice(value any, hosted bool) any {
	switch choice := value.(type) {
	case string:
		switch choice {
		case "required":
			return map[string]any{"type": "any"}
		case "none":
			return map[string]any{"type": "auto", "disable_parallel_tool_use": true}
		case "auto":
			return map[string]any{"type": "auto"}
		}
	case map[string]any:
		typ := stringValue(choice["type"])
		if typ == "allowed_tools" {
			_, mode, _ := allowedToolNames(choice)
			if mode == "required" {
				return map[string]any{"type": "any"}
			}
			return map[string]any{"type": "auto"}
		}
		if typ == "function" {
			name := stringValue(choice["name"])
			if (name == "web_search" || name == "x_search") && hosted {
				return map[string]any{"type": "tool", "name": "web_search"}
			}
			return map[string]any{"type": "tool", "name": name}
		}
		if (typ == "web_search" || typ == "x_search") && hosted {
			return map[string]any{"type": "tool", "name": "web_search"}
		}
	}
	return nil
}

func chatToolChoice(value any, hosted bool) any {
	if hosted && value == "required" {
		// Chat has no portable "any function or hosted search" choice. The
		// provider search extension is forced separately, so do not also force
		// an unrelated function call.
		return nil
	}
	choice, ok := value.(map[string]any)
	if !ok {
		return value
	}
	if stringValue(choice["type"]) == "allowed_tools" {
		_, mode, _ := allowedToolNames(choice)
		if mode == "required" {
			if hosted {
				return nil
			}
			return "required"
		}
		return "auto"
	}
	if stringValue(choice["type"]) == "function" {
		name := stringValue(choice["name"])
		if hosted && (name == "web_search" || name == "x_search") {
			return nil
		}
		return map[string]any{"type": "function", "function": map[string]any{"name": name}}
	}
	return nil
}

func hostedToolChoiceRequired(value any) bool {
	choice, ok := value.(map[string]any)
	if !ok {
		return value == "required"
	}
	typ := stringValue(choice["type"])
	if typ == "allowed_tools" {
		_, mode, _ := allowedToolNames(choice)
		return mode == "required" && toolChoiceAllowsHostedSearch(choice)
	}
	name := stringValue(choice["name"])
	return typ == "x_search" || isHostedWebSearchType(typ) ||
		(typ == "function" && (name == "web_search" || name == "x_search"))
}

func appendMessage(messages *[]any, role string, content any) {
	if role == "" {
		role = "user"
	}
	*messages = append(*messages, map[string]any{"role": role, "content": content})
}

func appendBlockToRole(messages *[]any, role string, block map[string]any) {
	if len(*messages) > 0 {
		last, _ := (*messages)[len(*messages)-1].(map[string]any)
		if stringValue(last["role"]) == role {
			content := anySlice(last["content"])
			if text, ok := last["content"].(string); ok && text != "" {
				content = []any{map[string]any{"type": "text", "text": text}}
			}
			last["content"] = append(content, block)
			return
		}
	}
	appendMessage(messages, role, []any{block})
}

func appendChatToolCall(messages *[]any, call map[string]any, pendingReasoning *[]string, assistantIndex *int) {
	if *assistantIndex >= 0 && *assistantIndex == len(*messages)-1 && len(*pendingReasoning) == 0 {
		last, _ := (*messages)[*assistantIndex].(map[string]any)
		if stringValue(last["role"]) == "assistant" {
			last["tool_calls"] = append(anySlice(last["tool_calls"]), call)
			return
		}
	}
	message := map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{call}}
	if len(*pendingReasoning) > 0 {
		message["reasoning_content"] = strings.Join(*pendingReasoning, "\n")
		*pendingReasoning = nil
	}
	*messages = append(*messages, message)
	*assistantIndex = len(*messages) - 1
}

func contentText(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	var parts []string
	for _, raw := range anySlice(value) {
		block, _ := raw.(map[string]any)
		if text := stringValue(block["text"]); text != "" {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "\n")
}

func responseOutputContent(value any, anthropic bool) any {
	if text, ok := value.(string); ok {
		return text
	}
	var blocks []any
	for _, raw := range anySlice(value) {
		part, _ := raw.(map[string]any)
		if part == nil {
			continue
		}
		switch stringValue(part["type"]) {
		case "input_text", "output_text", "text":
			blocks = append(blocks, map[string]any{"type": "text", "text": stringValue(part["text"])})
		case "input_image", "image_url":
			imageURL := stringValue(part["image_url"])
			if image, _ := part["image_url"].(map[string]any); imageURL == "" && image != nil {
				imageURL = stringValue(image["url"])
			}
			if imageURL == "" {
				continue
			}
			if anthropic {
				blocks = append(blocks, anthropicImageBlock(imageURL))
			} else {
				blocks = append(blocks, map[string]any{"type": "image_url", "image_url": map[string]any{"url": imageURL}})
			}
		}
	}
	if len(blocks) == 0 {
		encoded, err := json.Marshal(value)
		if err == nil {
			return string(encoded)
		}
		return ""
	}
	if len(blocks) == 1 {
		if block, _ := blocks[0].(map[string]any); block != nil && stringValue(block["type"]) == "text" {
			return stringValue(block["text"])
		}
	}
	return blocks
}

func lastUserText(input any) string {
	items := anySlice(input)
	for i := len(items) - 1; i >= 0; i-- {
		item, _ := items[i].(map[string]any)
		if stringValue(item["role"]) == "user" {
			return contentText(convertMessageContent(item["content"], false))
		}
	}
	if text, _ := input.(string); text != "" {
		return text
	}
	return ""
}

func anySlice(value any) []any {
	values, _ := value.([]any)
	return values
}

func stringValue(value any) string {
	s, _ := value.(string)
	return s
}

func firstString(values map[string]any, keys ...string) string {
	for _, key := range keys {
		if value := stringValue(values[key]); value != "" {
			return value
		}
	}
	return ""
}

func positiveInt(value any, fallback int) int {
	switch v := value.(type) {
	case json.Number:
		if n, err := v.Int64(); err == nil && n > 0 {
			return int(n)
		}
	case float64:
		if v > 0 {
			return int(v)
		}
	case int:
		if v > 0 {
			return v
		}
	}
	return fallback
}

func copyIfPresent(dst, src map[string]any, keys ...string) {
	for _, key := range keys {
		if value, ok := src[key]; ok && value != nil {
			dst[key] = value
		}
	}
}

func valueOr(value, fallback any) any {
	if value == nil {
		return fallback
	}
	return value
}
