package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/hellowind777/hellogrok/internal/appinfo"
	"github.com/hellowind777/hellogrok/internal/config"
	"github.com/hellowind777/hellogrok/internal/patch"
)

const maxFacadeBodyBytes int64 = 64 << 20

var errBodyTooLarge = errors.New("body exceeds size limit")

func (s *Server) forwardFacade(w http.ResponseWriter, incoming *http.Request, route config.Route) {
	defer s.flushReasoningProvenance()
	if incoming.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "custom channel facade accepts POST only")
		return
	}
	if !isJSONContentType(incoming.Header.Get("Content-Type")) {
		writeJSONError(w, http.StatusUnsupportedMediaType, "custom channel facade requires application/json")
		return
	}
	if incoming.ContentLength > maxFacadeBodyBytes {
		writeJSONError(w, http.StatusRequestEntityTooLarge, "request body exceeds 64 MiB")
		return
	}
	target, protocol, err := upstreamTarget(route, incoming.URL.RawQuery)
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, err.Error())
		return
	}
	body, err := readBodyLimited(incoming.Body, maxFacadeBodyBytes)
	_ = incoming.Body.Close()
	if err != nil {
		if errors.Is(err, errBodyTooLarge) {
			writeJSONError(w, http.StatusRequestEntityTooLarge, "request body exceeds 64 MiB")
			return
		}
		writeJSONError(w, http.StatusBadRequest, "read request: "+err.Error())
		return
	}
	request, err := adaptFacadeRequestWithReasoning(body, route, s.replays, s.reasoning, keepUnknownReasoning)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if request.Protocol != protocol {
		writeJSONError(w, http.StatusInternalServerError, "internal backend mismatch")
		return
	}
	if !routeHasCredential(route, incoming.Header) && !routeIsLoopback(route) {
		s.log.Printf("UP blocked channel=%s: no channel api_key/env_key; incoming authorization was not forwarded", route.ChannelID)
		writeJSONError(w, http.StatusUnauthorized, "custom channel has no channel-owned credential")
		return
	}

	tools, webSearch, hostedSearch, functionSearch, xSearch := summarizeBody(request.Body)
	logTarget := safeDiagnosticTarget(target)
	s.log.Printf("UP channel=%s backend=%s %s body=%dB model=%s tools=%d web_search=%d hosted_web_search=%d function_web_search=%d x_search=%d build_hosted_web_search=%d build_x_search=%d proxy_added_web_search=%t client_web_search_prepared=%t client_web_search_aliased=%t",
		route.ChannelID, route.APIBackend, logTarget, len(request.Body), route.WireModel, tools, webSearch, hostedSearch, functionSearch, xSearch,
		request.BuildHostedWebSearch, request.BuildXSearch, request.ProxyAddedWebSearch, request.ClientSearchPrepared, request.ClientSearchAlias != "")
	if request.Reasoning.Opaque > 0 {
		s.log.Printf("UP channel=%s reasoning projection opaque=%d compatible=%d unknown=%d dropped=%d recovery=%t",
			route.ChannelID, request.Reasoning.Opaque, request.Reasoning.Compatible,
			request.Reasoning.Unknown, request.Reasoning.Dropped, request.ReasoningRecovery)
	}
	saveLastRequestMeta(logTarget, route.WireModel, len(request.Body), tools, webSearch, hostedSearch, functionSearch, xSearch, request)
	if incomingModel := extractModel(body); incomingModel != "" && incomingModel != route.WireModel {
		s.log.Printf("UP model isolated channel=%s: %s -> %s", route.ChannelID, incomingModel, route.WireModel)
	}
	if hasIncomingCredential(incoming.Header) {
		if _, ok := incomingProviderCredential(route, incoming.Header); ok {
			s.log.Printf("UP auth isolated channel=%s: used credential from configured channel auth_provider", route.ChannelID)
		} else {
			s.log.Printf("UP auth isolated channel=%s: ignored incoming credential and used channel credential", route.ChannelID)
		}
	}

	started := time.Now()
	upstreamContext, cancelUpstream := context.WithCancel(incoming.Context())
	stopLifecycleCancel := context.AfterFunc(s.upstreamLifecycleContext(), cancelUpstream)
	defer func() {
		stopLifecycleCancel()
		cancelUpstream()
	}()
	doRequest := func(payload []byte) (*http.Response, error) {
		req, err := http.NewRequestWithContext(upstreamContext, http.MethodPost, target, bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		u, _ := url.Parse(target)
		req.Host = u.Host
		copySafeRequestHeaders(req.Header, incoming.Header)
		req.Header.Set("Content-Type", "application/json")
		if request.Stream {
			req.Header.Set("Accept", "text/event-stream, application/json")
		} else {
			req.Header.Set("Accept", "application/json")
		}
		if req.Header.Get("User-Agent") == "" {
			req.Header.Set("User-Agent", appinfo.Name+"/"+appinfo.Version)
		}
		applyRouteHeaders(req.Header, route, incoming.Header)
		if request.Protocol == wireMessages {
			if req.Header.Get("Anthropic-Version") == "" {
				req.Header.Set("Anthropic-Version", "2023-06-01")
			}
		}
		req.ContentLength = int64(len(payload))
		return s.client.Do(req)
	}

	var resp *http.Response
	for {
		resp, err = doRequest(request.Body)
		if err != nil {
			detail := safeUpstreamError(err)
			s.log.Printf("UP channel=%s request failed: %s", route.ChannelID, detail)
			writeRetryableJSONError(w, http.StatusBadGateway, "upstream: "+detail)
			return
		}
		s.log.Printf("UP channel=%s status=%d ct=%s %s", route.ChannelID, resp.StatusCode, resp.Header.Get("Content-Type"), time.Since(started).Round(time.Millisecond))
		if encoding := strings.TrimSpace(resp.Header.Get("Content-Encoding")); encoding != "" && !strings.EqualFold(encoding, "identity") {
			_ = resp.Body.Close()
			writeJSONError(w, http.StatusBadGateway, "upstream returned unsupported content encoding "+encoding)
			return
		}
		if resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices {
			break
		}
		if resp.StatusCode >= http.StatusMultipleChoices && resp.StatusCode < http.StatusBadRequest {
			_ = resp.Body.Close()
			writeJSONError(w, http.StatusBadGateway, "upstream redirects are not accepted")
			return
		}

		data, readErr := readBodyLimited(resp.Body, maxFacadeBodyBytes)
		_ = resp.Body.Close()
		if readErr != nil {
			if errors.Is(readErr, errBodyTooLarge) {
				writeJSONError(w, http.StatusBadGateway, "upstream error body exceeds 64 MiB")
			} else {
				writeRetryableJSONError(w, http.StatusBadGateway, "read upstream error response: "+readErr.Error())
			}
			return
		}

		reasoningRejected := isOpaqueReasoningRejection(resp.StatusCode, data)
		keptOpaqueReasoning := request.Reasoning.Opaque - request.Reasoning.Dropped
		if reasoningRejected && keptOpaqueReasoning > 0 && !request.ReasoningRecovery {
			retryRequest, retryErr := adaptFacadeRequestWithReasoning(body, route, s.replays, s.reasoning, dropAllOpaqueReasoning)
			if retryErr == nil && retryRequest.Reasoning.Dropped > request.Reasoning.Dropped {
				s.log.Printf("UP channel=%s reasoning recovery retry once removed=%d after status=%d",
					route.ChannelID, retryRequest.Reasoning.Dropped, resp.StatusCode)
				request = retryRequest
				saveLastRequestMeta(logTarget, route.WireModel, len(request.Body), tools, webSearch, hostedSearch, functionSearch, xSearch, request)
				continue
			}
			if retryErr != nil {
				s.log.Printf("UP channel=%s reasoning recovery request failed: %v", route.ChannelID, retryErr)
			}
		}

		copySafeResponseHeaders(w.Header(), resp.Header)
		if reasoningRejected {
			w.Header().Set("X-Should-Retry", "false")
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(data)
		return
	}
	defer resp.Body.Close()

	options := patch.Options{GPTResponses: true, WebSearch: true, RequestModel: route.WireModel}
	upstreamSSE := strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream")
	if upstreamSSE && !request.Stream {
		writeJSONError(w, http.StatusBadGateway, "upstream ignored stream=false; cannot return an event stream")
		return
	}
	if upstreamSSE {
		switch request.Protocol {
		case wireResponses:
			s.streamResponsesSSE(w, resp, route, request, options, started)
		case wireMessages:
			s.streamMessagesSSE(w, resp, route, request, started)
		case wireChatCompletions:
			s.streamChatSSE(w, resp, route, request, started)
		default:
			writeJSONError(w, http.StatusInternalServerError, "unsupported streaming backend")
		}
		return
	}

	if request.Protocol == wireResponses {
		data, readErr := readBodyLimited(resp.Body, maxFacadeBodyBytes)
		if readErr != nil {
			if errors.Is(readErr, errBodyTooLarge) {
				writeJSONError(w, http.StatusBadGateway, "upstream response body exceeds 64 MiB")
			} else {
				writeRetryableJSONError(w, http.StatusBadGateway, "read upstream response: "+readErr.Error())
			}
			return
		}
		if isHTMLContentType(resp.Header.Get("Content-Type")) {
			writeJSONError(w, http.StatusBadGateway, upstreamHTMLResponseMessage(route.APIBackend))
			return
		}
		data, readErr = restoreClientWebSearchAliasJSON(data, request.ClientSearchAlias)
		if readErr != nil {
			writeJSONError(w, http.StatusBadGateway, "invalid upstream Responses body while restoring client search: "+readErr.Error())
			return
		}
		data, readErr = patch.PatchJSONBytesStrict(data, options)
		if readErr != nil {
			writeJSONError(w, http.StatusBadGateway, "invalid upstream Responses body: "+readErr.Error())
			return
		}
		canonical, decodeErr := decodeJSONMap(data)
		if decodeErr != nil {
			writeJSONError(w, http.StatusBadGateway, "invalid upstream Responses body: "+decodeErr.Error())
			return
		}
		backfillResponseSearchSources(canonical, request.HostedWebSearch, request.SearchQuery)
		if validateErr := validateResponsesEnvelope(canonical); validateErr != nil {
			writeJSONError(w, http.StatusBadGateway, "invalid upstream Responses body: "+validateErr.Error())
			return
		}
		s.captureReasoningProvenance(route, canonical)
		data, readErr = json.Marshal(canonical)
		if readErr != nil {
			writeJSONError(w, http.StatusBadGateway, "encode upstream Responses body: "+readErr.Error())
			return
		}
		evidence := newSearchEvidence()
		evidence.observeJSON(data)
		s.logSearchEvidence(route.ChannelID, request, evidence)
		s.replays.captureJSON(route.ChannelID, request.ReplayScope, data)
		if request.Stream {
			s.log.Printf("UP channel=%s backend=responses ignored stream=true; emitting buffered JSON fallback", route.ChannelID)
			if writeErr := writeCanonicalResponse(w, canonical, true); writeErr != nil {
				writeJSONError(w, http.StatusBadGateway, "invalid non-stream Responses body: "+writeErr.Error())
			}
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(data)))
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(data)
		return
	}

	data, readErr := readBodyLimited(resp.Body, maxFacadeBodyBytes)
	if readErr != nil {
		if errors.Is(readErr, errBodyTooLarge) {
			writeJSONError(w, http.StatusBadGateway, "upstream response body exceeds 64 MiB")
		} else {
			writeRetryableJSONError(w, http.StatusBadGateway, "read upstream response: "+readErr.Error())
		}
		return
	}
	if isHTMLContentType(resp.Header.Get("Content-Type")) {
		writeJSONError(w, http.StatusBadGateway, upstreamHTMLResponseMessage(route.APIBackend))
		return
	}
	evidence := newSearchEvidence()
	evidence.observeJSON(data)
	s.logSearchEvidence(route.ChannelID, request, evidence)
	var result canonicalResult
	if request.Protocol == wireMessages {
		result, err = canonicalFromMessages(data, request.HostedWebSearch, request.SearchQuery)
	} else {
		result, err = canonicalFromChat(data, request.HostedWebSearch, request.SearchQuery)
	}
	if err != nil {
		s.log.Printf("UP channel=%s canonical response error: %v", route.ChannelID, err)
		writeJSONError(w, http.StatusBadGateway, "invalid upstream response: "+err.Error())
		return
	}
	if request.Protocol == wireMessages {
		s.replays.captureMessages(route.ChannelID, request.ReplayScope, data)
	}
	canonical := canonicalResponse(route, request, result)
	restoreClientWebSearchAlias(canonical, request.ClientSearchAlias)
	backfillResponseSearchSources(canonical, request.HostedWebSearch, request.SearchQuery)
	s.captureReasoningProvenance(route, canonical)
	if request.Stream {
		s.log.Printf("UP channel=%s backend=%s ignored stream=true; emitting buffered JSON fallback", route.ChannelID, route.APIBackend)
	}
	if writeErr := writeCanonicalResponse(w, canonical, request.Stream); writeErr != nil {
		writeJSONError(w, http.StatusBadGateway, "canonical response error: "+writeErr.Error())
	}
}

func readBodyLimited(reader io.Reader, limit int64) ([]byte, error) {
	limited := &io.LimitedReader{R: reader, N: limit + 1}
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errBodyTooLarge
	}
	return data, nil
}

func isJSONContentType(value string) bool {
	mediaType, _, err := mime.ParseMediaType(value)
	if err != nil {
		return false
	}
	mediaType = strings.ToLower(mediaType)
	return mediaType == "application/json" || strings.HasSuffix(mediaType, "+json")
}

func isHTMLContentType(value string) bool {
	mediaType, _, err := mime.ParseMediaType(value)
	return err == nil && (strings.EqualFold(mediaType, "text/html") || strings.EqualFold(mediaType, "application/xhtml+xml"))
}

func upstreamHTMLResponseMessage(backend string) string {
	return fmt.Sprintf("upstream returned HTML instead of a %s JSON response; check the channel base_url API prefix and api_backend", backend)
}

func isLoopbackRequestHost(value string) bool {
	host := strings.TrimSpace(value)
	if parsed, _, err := net.SplitHostPort(host); err == nil {
		host = parsed
	}
	host = strings.Trim(strings.ToLower(host), "[]")
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func routeIsLoopback(route config.Route) bool {
	return isLoopbackRequestHost(route.Host)
}

func routeHasCredential(route config.Route, incoming http.Header) bool {
	if strings.TrimSpace(route.APIKey) != "" ||
		headerValue(route.ExtraHeaders, "Authorization") != "" ||
		headerValue(route.ExtraHeaders, "X-Api-Key") != "" {
		return true
	}
	// Only an explicitly configured auth_provider may own an incoming token.
	// A custom channel without that declaration must never inherit login OAuth.
	_, ok := incomingProviderCredential(route, incoming)
	return ok
}

func routeUsesIncomingProviderAuth(route config.Route) bool {
	return route.DynamicAuth && strings.TrimSpace(route.APIKey) == "" &&
		headerValue(route.ExtraHeaders, "Authorization") == "" &&
		headerValue(route.ExtraHeaders, "X-Api-Key") == ""
}

func applyRouteHeaders(header http.Header, route config.Route, incoming http.Header) {
	header.Del("Authorization")
	header.Del("X-Api-Key")
	if route.APIKey != "" {
		if route.AuthScheme == "x_api_key" {
			header.Set("X-Api-Key", route.APIKey)
		} else {
			header.Set("Authorization", "Bearer "+route.APIKey)
		}
	}
	for key, value := range route.ExtraHeaders {
		header.Set(key, value)
	}
	if token, ok := incomingProviderCredential(route, incoming); ok &&
		header.Get("Authorization") == "" && header.Get("X-Api-Key") == "" {
		if route.AuthScheme == "x_api_key" {
			header.Set("X-Api-Key", token)
		} else {
			header.Set("Authorization", "Bearer "+token)
		}
	}
}

func incomingProviderCredential(route config.Route, incoming http.Header) (string, bool) {
	if !routeUsesIncomingProviderAuth(route) {
		return "", false
	}
	switch route.IncomingAuthScheme {
	case "x_api_key":
		value := strings.TrimSpace(incoming.Get("X-Api-Key"))
		return value, value != ""
	default:
		value := strings.TrimSpace(incoming.Get("Authorization"))
		if len(value) < len("Bearer ") || !strings.EqualFold(value[:len("Bearer ")], "Bearer ") {
			return "", false
		}
		value = strings.TrimSpace(value[len("Bearer "):])
		return value, value != ""
	}
}

func hasIncomingCredential(header http.Header) bool {
	return strings.TrimSpace(header.Get("Authorization")) != "" ||
		strings.TrimSpace(header.Get("X-Api-Key")) != "" ||
		strings.TrimSpace(header.Get("X-Xai-Token-Auth")) != ""
}

func headerValue(values map[string]string, name string) string {
	for key, value := range values {
		if strings.EqualFold(key, name) {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func safeDiagnosticTarget(target string) string {
	parsed, err := url.Parse(target)
	if err != nil {
		return "<invalid upstream URL>"
	}
	parsed.User = nil
	parsed.RawQuery = ""
	parsed.Fragment = ""
	path := strings.ToLower(strings.TrimRight(parsed.Path, "/"))
	switch {
	case strings.HasSuffix(path, "/chat/completions"):
		parsed.Path = "/.../chat/completions"
	case strings.HasSuffix(path, "/responses"):
		parsed.Path = "/.../responses"
	case strings.HasSuffix(path, "/messages"):
		parsed.Path = "/.../messages"
	default:
		parsed.Path = ""
	}
	parsed.RawPath = ""
	return parsed.String()
}

func safeUpstreamError(err error) string {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return fmt.Sprintf("%s %s: %v", urlErr.Op, safeDiagnosticTarget(urlErr.URL), urlErr.Err)
	}
	return err.Error()
}

func copySafeRequestHeaders(dst, src http.Header) {
	blocked := map[string]bool{
		"authorization": true, "proxy-authorization": true, "x-api-key": true,
		"x-xai-token-auth": true, "cookie": true, "set-cookie": true,
		"host": true, "content-length": true, "accept-encoding": true,
		"connection": true, "proxy-connection": true, "keep-alive": true,
		"te": true, "trailer": true, "transfer-encoding": true, "upgrade": true,
	}
	for _, named := range src.Values("Connection") {
		for _, key := range strings.Split(named, ",") {
			blocked[strings.ToLower(strings.TrimSpace(key))] = true
		}
	}
	for key, values := range src {
		if blocked[strings.ToLower(key)] {
			continue
		}
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

func copySafeResponseHeaders(dst, src http.Header) {
	blocked := map[string]bool{
		"content-length": true, "content-encoding": true, "transfer-encoding": true,
		"connection": true, "proxy-connection": true, "proxy-authenticate": true,
		"proxy-authorization": true, "keep-alive": true, "te": true,
		"trailer": true, "upgrade": true, "set-cookie": true, "location": true,
	}
	for _, named := range src.Values("Connection") {
		for _, key := range strings.Split(named, ",") {
			blocked[strings.ToLower(strings.TrimSpace(key))] = true
		}
	}
	for key, values := range src {
		if blocked[strings.ToLower(key)] {
			continue
		}
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}
