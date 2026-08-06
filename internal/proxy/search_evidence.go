package proxy

import (
	"bytes"
	"encoding/json"
	"strings"
)

type searchEvidence struct {
	searchCalls       map[string]struct{}
	completedCalls    map[string]struct{}
	queryCalls        map[string]struct{}
	webFetchCalls     map[string]struct{}
	anonymousCall     bool
	anonymousComplete bool
	anonymousQuery    bool
	anonymousFetch    bool
	sources           int
	annotations       int
	usageRequests     int
}

func newSearchEvidence() *searchEvidence {
	return &searchEvidence{
		searchCalls:    map[string]struct{}{},
		completedCalls: map[string]struct{}{},
		queryCalls:     map[string]struct{}{},
		webFetchCalls:  map[string]struct{}{},
	}
}

func (e *searchEvidence) observeJSON(data []byte) {
	if e == nil || len(bytes.TrimSpace(data)) == 0 {
		return
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var value any
	if dec.Decode(&value) != nil {
		return
	}
	frameSources := 0
	frameAnnotations := 0
	frameUsage := 0
	var walk func(any)
	walk = func(current any) {
		switch typed := current.(type) {
		case map[string]any:
			typ := strings.ToLower(strings.TrimSpace(stringValue(typed["type"])))
			switch typ {
			case "web_search_call":
				id := firstString(typed, "id", "call_id")
				e.addSearchCall(id)
				if strings.EqualFold(stringValue(typed["status"]), "completed") {
					e.addCompletedCall(id)
				}
				if action, _ := typed["action"].(map[string]any); action != nil &&
					(strings.TrimSpace(stringValue(action["query"])) != "" || firstSearchQuery(action["queries"]) != "") {
					e.addQueryCall(id)
				}
			case "server_tool_use":
				if strings.EqualFold(stringValue(typed["name"]), "web_search") {
					id := firstString(typed, "id", "call_id")
					e.addSearchCall(id)
					if input, _ := typed["input"].(map[string]any); input != nil &&
						(strings.TrimSpace(firstString(input, "query", "q")) != "") {
						e.addQueryCall(id)
					}
				}
			case "web_search_tool_result":
				id := firstString(typed, "tool_use_id", "id", "call_id")
				e.addSearchCall(id)
				e.addCompletedCall(id)
				frameSources += countMessagesSearchResults(typed["content"])
			case "function_call", "tool_use":
				if strings.EqualFold(stringValue(typed["name"]), "web_fetch") {
					e.addWebFetchCall(firstString(typed, "call_id", "id"))
				}
			}

			for key, child := range typed {
				switch strings.ToLower(key) {
				case "sources", "search_results", "web_search_results":
					frameSources += collectionLength(child)
				case "annotations", "citations":
					frameAnnotations += collectionLength(child)
				case "usage":
					if count := searchUsageCount(child); count > frameUsage {
						frameUsage = count
					}
				}
				walk(child)
			}
		case []any:
			for _, child := range typed {
				walk(child)
			}
		}
	}
	walk(value)
	if frameSources > e.sources {
		e.sources = frameSources
	}
	if frameAnnotations > e.annotations {
		e.annotations = frameAnnotations
	}
	if frameUsage > e.usageRequests {
		e.usageRequests = frameUsage
	}
}

func (e *searchEvidence) addSearchCall(id string) {
	if id == "" {
		e.anonymousCall = true
		return
	}
	e.searchCalls[id] = struct{}{}
}

func (e *searchEvidence) addCompletedCall(id string) {
	if id == "" {
		e.anonymousComplete = true
		return
	}
	e.completedCalls[id] = struct{}{}
}

func (e *searchEvidence) addQueryCall(id string) {
	if id == "" {
		e.anonymousQuery = true
		return
	}
	e.queryCalls[id] = struct{}{}
}

func (e *searchEvidence) addWebFetchCall(id string) {
	if id == "" {
		e.anonymousFetch = true
		return
	}
	e.webFetchCalls[id] = struct{}{}
}

func (e *searchEvidence) counts() (calls, completed, queries, sources, annotations, usage, fetches int) {
	calls = len(e.searchCalls)
	completed = len(e.completedCalls)
	queries = len(e.queryCalls)
	fetches = len(e.webFetchCalls)
	if e.anonymousCall {
		calls++
	}
	if e.anonymousComplete {
		completed++
	}
	if e.anonymousQuery {
		queries++
	}
	if e.anonymousFetch {
		fetches++
	}
	if calls == 0 && (e.sources > 0 || e.annotations > 0 || e.usageRequests > 0) {
		calls = 1
	}
	if completed == 0 && (e.sources > 0 || e.annotations > 0 || e.usageRequests > 0) {
		completed = 1
	}
	return calls, completed, queries, e.sources, e.annotations, e.usageRequests, fetches
}

func (s *Server) logSearchEvidence(channel string, request facadeRequest, evidence *searchEvidence) {
	if evidence == nil {
		return
	}
	calls, completed, queries, sources, annotations, usage, fetches := evidence.counts()
	s.log.Printf("UP channel=%s search evidence declared=%t calls=%d completed=%d queries=%d sources=%d annotations=%d usage_requests=%d web_fetch_calls=%d",
		channel, request.HostedWebSearch, calls, completed, queries, sources, annotations, usage, fetches)
}

func countMessagesSearchResults(value any) int {
	count := 0
	for _, raw := range anySlice(value) {
		result, _ := raw.(map[string]any)
		if strings.EqualFold(stringValue(result["type"]), "web_search_result") {
			count++
		}
	}
	return count
}

func collectionLength(value any) int {
	switch typed := value.(type) {
	case []any:
		return len(typed)
	case map[string]any:
		return len(typed)
	case string:
		if strings.TrimSpace(typed) != "" {
			return 1
		}
	}
	return 0
}

func searchUsageCount(value any) int {
	best := 0
	var walk func(any)
	walk = func(current any) {
		switch typed := current.(type) {
		case map[string]any:
			for key, child := range typed {
				switch strings.ToLower(key) {
				case "num_sources_used", "web_search_requests", "search_requests", "search_queries_count":
					if count := numberInt(child); count > best {
						best = count
					}
				}
				walk(child)
			}
		case []any:
			for _, child := range typed {
				walk(child)
			}
		}
	}
	walk(value)
	return best
}
