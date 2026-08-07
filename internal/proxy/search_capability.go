package proxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hellowind777/hellogrok/internal/appinfo"
	"github.com/hellowind777/hellogrok/internal/config"
)

type CapabilityState string

const (
	CapabilitySupported   CapabilityState = "supported"
	CapabilityUnsupported CapabilityState = "unsupported"
	CapabilityUnknown     CapabilityState = "unknown"
)

type SearchToolCapability struct {
	State       CapabilityState
	Source      string
	ChatDialect config.ChatSearchDialect
}

type SearchCapabilities struct {
	WebSearch SearchToolCapability
	XSearch   SearchToolCapability
}

const (
	searchCapabilityCacheVersion = 2
	searchCapabilityConcurrency  = 4
	searchCapabilityProbeTimeout = 20 * time.Second
	maxCapabilityResponseBytes   = 8 << 20
	maxCapabilityErrorBytes      = 1 << 20
)

type searchCapabilityKind string

const (
	searchCapabilityWeb searchCapabilityKind = "web_search"
	searchCapabilityX   searchCapabilityKind = "x_search"
)

type searchCapabilityCache struct {
	Version int                                   `json:"version"`
	Salt    string                                `json:"salt"`
	Entries map[string]searchCapabilityCacheEntry `json:"entries"`
}

type searchCapabilityCacheEntry struct {
	WebSearch searchCapabilityCacheValue `json:"web_search,omitempty"`
	XSearch   searchCapabilityCacheValue `json:"x_search,omitempty"`
}

type searchCapabilityCacheValue struct {
	State       CapabilityState          `json:"state,omitempty"`
	CheckedAt   time.Time                `json:"checked_at,omitempty"`
	ChatDialect config.ChatSearchDialect `json:"chat_dialect,omitempty"`
}

type searchCapabilityProbeJob struct {
	key        string
	kind       searchCapabilityKind
	route      config.Route
	channels   []string
	capability SearchToolCapability
	checkedAt  time.Time
}

func SearchCapabilityCachePath(dataDir string) string {
	return filepath.Join(dataDir, "search_capabilities.json")
}

// RouteLooksLikeGrok reports whether a custom route identifies a Grok model.
// The same predicate controls Responses x_search normalization.
func RouteLooksLikeGrok(route config.Route) bool {
	return isGrokRoute(route)
}

// DetectSearchCapabilities probes hosted tools through the exact protocol,
// auth, header, URL, and request-conversion path used by the facade. Results
// are conservative: success requires structured execution evidence, while
// transient and authentication failures remain unknown.
func (s *Server) DetectSearchCapabilities(
	ctx context.Context,
	routes []config.Route,
	cachePath string,
	probeX bool,
) map[string]SearchCapabilities {
	now := time.Now().UTC()
	cache, cacheErr := loadSearchCapabilityCache(cachePath)
	if cacheErr != nil {
		s.log.Printf("search capability cache reset: %v", cacheErr)
	}
	if cache == nil {
		cache, cacheErr = newSearchCapabilityCache()
		if cacheErr != nil {
			s.log.Printf("search capability cache disabled: %v", cacheErr)
		}
	}

	reports := make(map[string]SearchCapabilities, len(routes))
	jobsByID := make(map[string]*searchCapabilityProbeJob)
	cacheChanged := false
	for _, route := range routes {
		channel := strings.TrimSpace(route.ChannelID)
		if channel == "" {
			continue
		}
		reports[channel] = SearchCapabilities{
			WebSearch: SearchToolCapability{State: CapabilityUnknown, Source: "not-probed"},
			XSearch:   SearchToolCapability{State: CapabilityUnknown, Source: "not-probed"},
		}

		canAuthenticate := !routeUsesIncomingProviderAuth(route) &&
			(routeHasCredential(route, http.Header{}) || routeIsLoopback(route))
		if !canAuthenticate {
			setSearchCapability(reports, channel, searchCapabilityWeb, SearchToolCapability{
				State: CapabilityUnknown, Source: "no-static-credential",
			})
		} else {
			s.prepareSearchCapabilityJob(cache, reports, jobsByID, route, searchCapabilityWeb, now)
		}

		if !probeX || !isGrokRoute(route) {
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(route.APIBackend), "responses") {
			setSearchCapability(reports, channel, searchCapabilityX, SearchToolCapability{
				State: CapabilityUnsupported, Source: "protocol-boundary",
			})
			continue
		}
		if !canAuthenticate {
			setSearchCapability(reports, channel, searchCapabilityX, SearchToolCapability{
				State: CapabilityUnknown, Source: "no-static-credential",
			})
			continue
		}
		s.prepareSearchCapabilityJob(cache, reports, jobsByID, route, searchCapabilityX, now)
	}

	jobs := make([]*searchCapabilityProbeJob, 0, len(jobsByID))
	for _, job := range jobsByID {
		jobs = append(jobs, job)
	}
	sort.Slice(jobs, func(i, j int) bool {
		if jobs[i].route.ChannelID == jobs[j].route.ChannelID {
			return jobs[i].kind < jobs[j].kind
		}
		return jobs[i].route.ChannelID < jobs[j].route.ChannelID
	})
	if len(jobs) > 0 {
		workers := searchCapabilityConcurrency
		if len(jobs) < workers {
			workers = len(jobs)
		}
		queue := make(chan *searchCapabilityProbeJob)
		var wg sync.WaitGroup
		for range workers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for job := range queue {
					probeCtx, cancel := context.WithTimeout(ctx, searchCapabilityProbeTimeout)
					job.capability = s.probeSearchCapability(probeCtx, job.route, job.kind)
					cancel()
					job.checkedAt = time.Now().UTC()
				}
			}()
		}
		for _, job := range jobs {
			queue <- job
		}
		close(queue)
		wg.Wait()

		for _, job := range jobs {
			for _, channel := range job.channels {
				setSearchCapability(reports, channel, job.kind, job.capability)
			}
			if cache != nil {
				cache.set(job.key, job.kind, job.capability, job.checkedAt)
				cacheChanged = true
			}
		}
	}

	if cache != nil && cache.prune(now) {
		cacheChanged = true
	}
	if cache != nil && cacheChanged {
		if err := writeSearchCapabilityCache(cachePath, cache); err != nil {
			s.log.Printf("search capability cache write failed: %v", err)
		}
	}
	return reports
}

func (s *Server) prepareSearchCapabilityJob(
	cache *searchCapabilityCache,
	reports map[string]SearchCapabilities,
	jobs map[string]*searchCapabilityProbeJob,
	route config.Route,
	kind searchCapabilityKind,
	now time.Time,
) {
	channel := strings.TrimSpace(route.ChannelID)
	if cache == nil {
		id := channel + "\x00" + string(kind)
		jobs[id] = &searchCapabilityProbeJob{key: id, kind: kind, route: route, channels: []string{channel}}
		return
	}
	key, err := cache.routeKey(route)
	if err != nil {
		setSearchCapability(reports, channel, kind, SearchToolCapability{State: CapabilityUnknown, Source: "cache-key-error"})
		s.log.Printf("search capability key failed channel=%s: %v", channel, err)
		return
	}
	if value, ok := cache.get(key, kind, now); ok {
		setSearchCapability(reports, channel, kind, SearchToolCapability{
			State: value.State, Source: "cache", ChatDialect: value.ChatDialect,
		})
		return
	}
	jobID := key + "\x00" + string(kind)
	if existing := jobs[jobID]; existing != nil {
		existing.channels = append(existing.channels, channel)
	} else {
		jobs[jobID] = &searchCapabilityProbeJob{key: key, kind: kind, route: route, channels: []string{channel}}
	}
}

func setSearchCapability(
	reports map[string]SearchCapabilities,
	channel string,
	kind searchCapabilityKind,
	capability SearchToolCapability,
) {
	report := reports[channel]
	if kind == searchCapabilityX {
		report.XSearch = capability
	} else {
		report.WebSearch = capability
	}
	reports[channel] = report
}

func (s *Server) probeSearchCapability(ctx context.Context, route config.Route, kind searchCapabilityKind) SearchToolCapability {
	if kind == searchCapabilityWeb && strings.EqualFold(strings.TrimSpace(route.APIBackend), "chat_completions") {
		return s.probeChatSearchCapability(ctx, route)
	}
	return s.probeSearchCapabilityOnce(ctx, route, kind)
}

func (s *Server) probeChatSearchCapability(ctx context.Context, route config.Route) SearchToolCapability {
	primary := chatSearchDialect(route)
	secondary := config.ChatSearchDialectWebSearchOptions
	if primary == secondary {
		secondary = config.ChatSearchDialectSearchParameters
	}
	var unknown *SearchToolCapability
	for _, dialect := range []config.ChatSearchDialect{primary, secondary} {
		candidate := route
		candidate.HostedChatSearchDialect = dialect
		result := s.probeSearchCapabilityOnce(ctx, candidate, searchCapabilityWeb)
		if result.State == CapabilitySupported {
			result.ChatDialect = dialect
			return result
		}
		if result.State == CapabilityUnknown && unknown == nil {
			copy := result
			unknown = &copy
		}
	}
	if unknown != nil {
		return *unknown
	}
	return SearchToolCapability{State: CapabilityUnsupported, Source: "probe"}
}

func (s *Server) probeSearchCapabilityOnce(ctx context.Context, route config.Route, kind searchCapabilityKind) SearchToolCapability {
	capabilities := hostedSearchCapabilities{}
	toolType := "web_search"
	prompt := "Use web search exactly once to find the official Go programming language website, then return a short answer."
	if kind == searchCapabilityX {
		capabilities.X = true
		toolType = "x_search"
		prompt = "Use X search exactly once to find the official xAI account, then return a short answer."
	} else {
		capabilities.Web = true
	}
	probeRoute := route
	probeRoute.SupportsBackendSearch = true
	probeRoute.HostedSearchKnown = true
	probeRoute.HostedWebSearch = capabilities.Web
	probeRoute.HostedXSearch = capabilities.X

	root := map[string]any{
		"model":             route.WireModel,
		"input":             prompt,
		"stream":            false,
		"store":             false,
		"max_output_tokens": 512,
		"tools":             []any{map[string]any{"type": toolType}},
		"tool_choice":       "required",
	}
	body, err := json.Marshal(root)
	if err != nil {
		return SearchToolCapability{State: CapabilityUnknown, Source: "probe-build-error"}
	}
	request, err := adaptFacadeRequest(body, probeRoute, newSearchReplayCache())
	if err != nil {
		s.log.Printf("search capability probe request failed channel=%s tool=%s: %v", route.ChannelID, kind, err)
		return SearchToolCapability{State: CapabilityUnknown, Source: "probe-build-error"}
	}
	target, protocol, err := upstreamTarget(route, "")
	if err != nil || request.Protocol != protocol {
		return SearchToolCapability{State: CapabilityUnknown, Source: "probe-target-error"}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(request.Body))
	if err != nil {
		return SearchToolCapability{State: CapabilityUnknown, Source: "probe-build-error"}
	}
	if parsed, parseErr := url.Parse(target); parseErr == nil {
		req.Host = parsed.Host
	}
	req.Header.Set("Content-Type", "application/json")
	if protocol == wireResponses {
		req.Header.Set("Accept", "application/json, text/event-stream")
	} else {
		req.Header.Set("Accept", "application/json")
	}
	req.Header.Set("User-Agent", appinfo.Name+"/"+appinfo.Version)
	applyRouteHeaders(req.Header, route, http.Header{})
	if protocol == wireMessages {
		req.Header.Set("Anthropic-Version", "2023-06-01")
	}
	req.ContentLength = int64(len(request.Body))

	resp, err := s.client.Do(req)
	if err != nil {
		return SearchToolCapability{State: CapabilityUnknown, Source: "probe-network"}
	}
	defer resp.Body.Close()
	if encoding := strings.TrimSpace(resp.Header.Get("Content-Encoding")); encoding != "" && !strings.EqualFold(encoding, "identity") {
		return SearchToolCapability{State: CapabilityUnknown, Source: "probe-encoding"}
	}
	limit := int64(maxCapabilityResponseBytes)
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		limit = maxCapabilityErrorBytes
	}
	data, readErr := readBodyLimited(resp.Body, limit)
	if readErr != nil {
		return SearchToolCapability{State: CapabilityUnknown, Source: "probe-read"}
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		if (resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnprocessableEntity) &&
			explicitUnsupportedSearchError(data, kind, protocol) {
			return SearchToolCapability{State: CapabilityUnsupported, Source: "probe"}
		}
		return SearchToolCapability{State: CapabilityUnknown, Source: "probe-http"}
	}
	if hasStructuredSearchEvidence(data, resp.Header.Get("Content-Type"), kind) {
		return SearchToolCapability{State: CapabilitySupported, Source: "probe"}
	}
	return SearchToolCapability{State: CapabilityUnknown, Source: "probe-no-evidence"}
}

func explicitUnsupportedSearchError(data []byte, kind searchCapabilityKind, protocol wireProtocol) bool {
	text := strings.ToLower(string(data))
	toolMarkers := []string{string(kind)}
	if kind == searchCapabilityWeb {
		toolMarkers = append(toolMarkers, "web_search_20250305")
		switch protocol {
		case wireChatCompletions:
			toolMarkers = append(toolMarkers, "web_search_options", "search_parameters")
		case wireMessages:
			toolMarkers = append(toolMarkers, "server tool")
		}
	}
	mentionsTool := false
	for _, marker := range toolMarkers {
		if strings.Contains(text, marker) {
			mentionsTool = true
			break
		}
	}
	if !mentionsTool {
		return false
	}
	for _, marker := range []string{
		"unknown tool", "unrecognized tool", "unsupported tool", "tool is not supported",
		"does not support", "not supported", "invalid tool", "invalid value", "unknown field",
		"unrecognized field", "extra inputs are not permitted", "extra_forbidden",
	} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

func hasStructuredSearchEvidence(data []byte, contentType string, kind searchCapabilityKind) bool {
	payloads := [][]byte{data}
	if strings.Contains(strings.ToLower(contentType), "text/event-stream") || bytes.Contains(data, []byte("data:")) {
		payloads = sseJSONPayloads(data)
	}
	for _, payload := range payloads {
		if kind == searchCapabilityX {
			if hasXSearchEvidence(payload) {
				return true
			}
			continue
		}
		evidence := newSearchEvidence()
		evidence.observeJSON(payload)
		calls, completed, queries, sources, annotations, usage, _ := evidence.counts()
		if calls > 0 || completed > 0 || queries > 0 || sources > 0 || annotations > 0 || usage > 0 {
			return true
		}
	}
	return false
}

func sseJSONPayloads(data []byte) [][]byte {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 64*1024), maxCapabilityResponseBytes)
	var payloads [][]byte
	var frame []string
	flush := func() {
		payload, ok := sseFramePayload(frame)
		frame = frame[:0]
		if !ok || strings.TrimSpace(payload) == "" || strings.TrimSpace(payload) == "[DONE]" {
			return
		}
		payloads = append(payloads, []byte(payload))
	}
	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if line == "" {
			flush()
			continue
		}
		frame = append(frame, line)
	}
	if len(frame) > 0 {
		flush()
	}
	return payloads
}

func hasXSearchEvidence(data []byte) bool {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var value any
	if dec.Decode(&value) != nil {
		return false
	}
	var walk func(any) bool
	walk = func(current any) bool {
		switch typed := current.(type) {
		case map[string]any:
			typ := strings.ToLower(strings.TrimSpace(stringValue(typed["type"])))
			name := strings.ToLower(strings.TrimSpace(stringValue(typed["name"])))
			if typ == "x_search_call" ||
				(typ == "custom_tool_call" && (strings.HasPrefix(name, "x_") || name == "x_search")) {
				return true
			}
			for _, child := range typed {
				if walk(child) {
					return true
				}
			}
		case []any:
			for _, child := range typed {
				if walk(child) {
					return true
				}
			}
		}
		return false
	}
	return walk(value)
}

func newSearchCapabilityCache() (*searchCapabilityCache, error) {
	salt := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, salt); err != nil {
		return nil, fmt.Errorf("generate cache salt: %w", err)
	}
	return &searchCapabilityCache{
		Version: searchCapabilityCacheVersion,
		Salt:    base64.RawStdEncoding.EncodeToString(salt),
		Entries: map[string]searchCapabilityCacheEntry{},
	}, nil
}

func loadSearchCapabilityCache(path string) (*searchCapabilityCache, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var cache searchCapabilityCache
	if err := json.Unmarshal(data, &cache); err != nil {
		return nil, fmt.Errorf("parse cache: %w", err)
	}
	if cache.Version != searchCapabilityCacheVersion {
		return nil, fmt.Errorf("unsupported cache version %d", cache.Version)
	}
	if _, err := base64.RawStdEncoding.DecodeString(cache.Salt); err != nil {
		return nil, fmt.Errorf("invalid cache salt")
	}
	if cache.Entries == nil {
		cache.Entries = map[string]searchCapabilityCacheEntry{}
	}
	return &cache, nil
}

func (c *searchCapabilityCache) routeKey(route config.Route) (string, error) {
	salt, err := base64.RawStdEncoding.DecodeString(c.Salt)
	if err != nil || len(salt) < 16 {
		return "", fmt.Errorf("invalid cache salt")
	}
	target, protocol, err := upstreamTarget(route, "")
	if err != nil {
		return "", err
	}
	parsed, err := url.Parse(target)
	if err != nil {
		return "", err
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	parsed.Host = strings.ToLower(parsed.Host)
	parsed.Fragment = ""

	type headerPair struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	}
	headers := make([]headerPair, 0, len(route.ExtraHeaders))
	for name, value := range route.ExtraHeaders {
		headers = append(headers, headerPair{Name: strings.ToLower(strings.TrimSpace(name)), Value: value})
	}
	sort.Slice(headers, func(i, j int) bool {
		if headers[i].Name == headers[j].Name {
			return headers[i].Value < headers[j].Value
		}
		return headers[i].Name < headers[j].Name
	})
	identity := struct {
		Target     string       `json:"target"`
		Protocol   wireProtocol `json:"protocol"`
		Model      string       `json:"model"`
		AuthScheme string       `json:"auth_scheme"`
		APIKey     string       `json:"api_key"`
		Headers    []headerPair `json:"headers"`
	}{
		Target:     parsed.String(),
		Protocol:   protocol,
		Model:      route.WireModel,
		AuthScheme: route.AuthScheme,
		APIKey:     route.APIKey,
		Headers:    headers,
	}
	payload, err := json.Marshal(identity)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, salt)
	_, _ = mac.Write(payload)
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

func (c *searchCapabilityCache) get(key string, kind searchCapabilityKind, now time.Time) (searchCapabilityCacheValue, bool) {
	entry, ok := c.Entries[key]
	if !ok {
		return searchCapabilityCacheValue{}, false
	}
	value := entry.WebSearch
	if kind == searchCapabilityX {
		value = entry.XSearch
	}
	if !validCapabilityState(value.State) || value.CheckedAt.IsZero() || now.Sub(value.CheckedAt) > capabilityTTL(value.State) ||
		(value.ChatDialect != "" && !validChatSearchDialect(value.ChatDialect)) {
		return searchCapabilityCacheValue{}, false
	}
	return value, true
}

func (c *searchCapabilityCache) set(key string, kind searchCapabilityKind, capability SearchToolCapability, checkedAt time.Time) {
	if !validCapabilityState(capability.State) {
		capability.State = CapabilityUnknown
	}
	if kind != searchCapabilityWeb || capability.State != CapabilitySupported ||
		!validChatSearchDialect(capability.ChatDialect) {
		capability.ChatDialect = ""
	}
	entry := c.Entries[key]
	value := searchCapabilityCacheValue{
		State: capability.State, CheckedAt: checkedAt.UTC(), ChatDialect: capability.ChatDialect,
	}
	if kind == searchCapabilityX {
		entry.XSearch = value
	} else {
		entry.WebSearch = value
	}
	c.Entries[key] = entry
}

func (c *searchCapabilityCache) prune(now time.Time) bool {
	changed := false
	for key, entry := range c.Entries {
		if cacheValueExpired(entry.WebSearch, now) {
			entry.WebSearch = searchCapabilityCacheValue{}
			changed = true
		}
		if cacheValueExpired(entry.XSearch, now) {
			entry.XSearch = searchCapabilityCacheValue{}
			changed = true
		}
		if entry.WebSearch.State == "" && entry.XSearch.State == "" {
			delete(c.Entries, key)
			continue
		}
		c.Entries[key] = entry
	}
	return changed
}

func cacheValueExpired(value searchCapabilityCacheValue, now time.Time) bool {
	if value.State == "" {
		return false
	}
	return !validCapabilityState(value.State) || value.CheckedAt.IsZero() || now.Sub(value.CheckedAt) > capabilityTTL(value.State)
}

func validCapabilityState(state CapabilityState) bool {
	return state == CapabilitySupported || state == CapabilityUnsupported || state == CapabilityUnknown
}

func validChatSearchDialect(dialect config.ChatSearchDialect) bool {
	return dialect == config.ChatSearchDialectSearchParameters ||
		dialect == config.ChatSearchDialectWebSearchOptions
}

func capabilityTTL(state CapabilityState) time.Duration {
	switch state {
	case CapabilitySupported:
		return 24 * time.Hour
	case CapabilityUnsupported:
		return 6 * time.Hour
	default:
		return 10 * time.Minute
	}
}

func writeSearchCapabilityCache(path string, cache *searchCapabilityCache) error {
	data, err := json.MarshalIndent(cache, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".hellogrok-search-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	committed := false
	defer func() {
		_ = tmp.Close()
		if !committed {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := replaceSearchCapabilityFile(tmpPath, path); err != nil {
		return err
	}
	committed = true
	return nil
}
