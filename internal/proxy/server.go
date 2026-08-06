package proxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/hellowind777/hellogrok/internal/config"
	"github.com/hellowind777/hellogrok/internal/patch"
)

// Server is a channel-isolated Responses facade. Grok Build always talks
// Responses to /c/<channel>/responses; the facade converts to the channel's
// original upstream protocol.
type Server struct {
	PathAddr string

	mu       sync.RWMutex
	channels map[string]config.Route
	log      *log.Logger

	pathLn        net.Listener
	pathServer    *http.Server
	wg            sync.WaitGroup
	lifecycleMu   sync.RWMutex
	requestCtx    context.Context
	requestCancel context.CancelFunc

	transport       *http.Transport
	client          *http.Client
	connections     *connectionTracker
	shutdownTimeout time.Duration

	probedMu sync.Mutex
	probed   map[string]bool
	replays  *searchReplayCache
}

const maxSSEEventBytes = 16 << 20

func New(logger *log.Logger) *Server {
	if logger == nil {
		logger = log.Default()
	}
	connections := newConnectionTracker()
	transport := newUpstreamTransport(connections)
	requestCtx, requestCancel := context.WithCancel(context.Background())
	return &Server{
		PathAddr:    "127.0.0.1:18787",
		channels:    map[string]config.Route{},
		log:         logger,
		transport:   transport,
		connections: connections,
		client: &http.Client{
			Transport: transport,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		shutdownTimeout: 5 * time.Second,
		requestCtx:      requestCtx,
		requestCancel:   requestCancel,
		probed:          map[string]bool{},
		replays:         newSearchReplayCache(),
	}
}

func (s *Server) SetRoutes(routes []config.Route) {
	s.mu.Lock()
	s.channels = make(map[string]config.Route, len(routes))
	for _, route := range routes {
		if id := strings.TrimSpace(route.ChannelID); id != "" {
			s.channels[id] = route
		}
	}
	s.mu.Unlock()
}

func (s *Server) lookupChannel(id string) (config.Route, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	route, ok := s.channels[strings.TrimSpace(id)]
	return route, ok
}

func (s *Server) StartPath() error {
	if err := s.ReservePath(); err != nil {
		return err
	}
	if err := s.ServePath(); err != nil {
		s.Stop()
		return err
	}
	return nil
}

// ReservePath claims the facade address without accepting requests. Startup
// uses this to establish single-instance ownership before recovering config.
func (s *Server) ReservePath() error {
	purgeLegacyRequestDiagnostics()
	if s.pathLn != nil {
		return fmt.Errorf("local facade address is already reserved")
	}
	listener, err := net.Listen("tcp", s.PathAddr)
	if err != nil {
		return err
	}
	s.pathLn = listener
	return nil
}

// ServePath starts accepting requests on the previously reserved address.
func (s *Server) ServePath() error {
	if s.pathLn == nil {
		return fmt.Errorf("local facade address is not reserved")
	}
	if s.pathServer != nil {
		return fmt.Errorf("local facade is already serving")
	}
	s.lifecycleMu.Lock()
	if s.requestCtx == nil || s.requestCtx.Err() != nil {
		s.requestCtx, s.requestCancel = context.WithCancel(context.Background())
	}
	s.connections.Open()
	s.lifecycleMu.Unlock()
	s.pathServer = &http.Server{
		Handler:           http.HandlerFunc(s.servePath),
		ReadHeaderTimeout: 15 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       90 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
	server := s.pathServer
	listener := s.pathLn
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
			s.log.Printf("local facade stopped unexpectedly: %v", err)
		}
	}()
	return nil
}

func (s *Server) Stop() {
	s.lifecycleMu.Lock()
	if s.requestCancel != nil {
		s.requestCancel()
	}
	s.connections.CloseAll()
	s.lifecycleMu.Unlock()
	server := s.pathServer
	listener := s.pathLn
	if server != nil {
		timeout := s.shutdownTimeout
		if timeout <= 0 {
			timeout = 5 * time.Second
		}
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		if err := server.Shutdown(ctx); err != nil {
			s.log.Printf("local facade graceful shutdown failed: %v; closing active connections", err)
			_ = server.Close()
		}
		cancel()
	}
	if listener != nil {
		_ = listener.Close()
	}
	s.transport.CloseIdleConnections()
	s.wg.Wait()
	if s.pathServer == server {
		s.pathServer = nil
	}
	if s.pathLn == listener {
		s.pathLn = nil
	}
}

func (s *Server) upstreamLifecycleContext() context.Context {
	s.lifecycleMu.RLock()
	defer s.lifecycleMu.RUnlock()
	if s.requestCtx == nil {
		return context.Background()
	}
	return s.requestCtx
}

func (s *Server) servePath(w http.ResponseWriter, request *http.Request) {
	if !isLoopbackRequestHost(request.Host) {
		writeJSONError(w, http.StatusMisdirectedRequest, "local facade requires a loopback Host header")
		return
	}
	if strings.TrimSpace(request.Header.Get("Origin")) != "" {
		writeJSONError(w, http.StatusForbidden, "browser-origin requests are not accepted")
		return
	}
	channelID, ok := channelFromPath(request.URL.EscapedPath())
	if !ok {
		writeJSONError(w, http.StatusNotFound, "unknown proxy route")
		return
	}
	route, found := s.lookupChannel(channelID)
	if !found {
		writeJSONError(w, http.StatusNotFound, "unknown custom channel")
		return
	}
	s.forwardFacade(w, request, route)
}
func newUpstreamTransport(connections *connectionTracker) *http.Transport {
	dialer := &net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
	}
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			conn, err := dialer.DialContext(ctx, network, address)
			if err != nil {
				return nil, err
			}
			return connections.Track(conn)
		},
		ForceAttemptHTTP2:     false,
		TLSNextProto:          map[string]func(string, *tls.Conn) http.RoundTripper{},
		MaxIdleConns:          64,
		MaxIdleConnsPerHost:   16,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   20 * time.Second,
		ResponseHeaderTimeout: 0,
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
			NextProtos: []string{"http/1.1"},
		},
	}
	return transport
}

// connectionTracker makes stopping the local facade also terminate requests
// that are blocked in an upstream server. The closed gate prevents a dial that
// races with Stop from escaping the shutdown sweep.
type connectionTracker struct {
	mu     sync.Mutex
	closed bool
	conns  map[*trackedConn]struct{}
}

type trackedConn struct {
	net.Conn
	owner *connectionTracker
	once  sync.Once
}

func newConnectionTracker() *connectionTracker {
	return &connectionTracker{conns: make(map[*trackedConn]struct{})}
}

func (t *connectionTracker) Open() {
	t.mu.Lock()
	t.closed = false
	t.mu.Unlock()
}

func (t *connectionTracker) Track(conn net.Conn) (net.Conn, error) {
	tracked := &trackedConn{Conn: conn, owner: t}
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		_ = conn.Close()
		return nil, net.ErrClosed
	}
	t.conns[tracked] = struct{}{}
	t.mu.Unlock()
	return tracked, nil
}

func (t *connectionTracker) CloseAll() {
	t.mu.Lock()
	t.closed = true
	connections := make([]*trackedConn, 0, len(t.conns))
	for conn := range t.conns {
		connections = append(connections, conn)
	}
	clear(t.conns)
	t.mu.Unlock()

	for _, conn := range connections {
		_ = conn.Close()
	}
}

func (t *connectionTracker) remove(conn *trackedConn) {
	t.mu.Lock()
	delete(t.conns, conn)
	t.mu.Unlock()
}

func (c *trackedConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() { c.owner.remove(c) })
	return err
}

func writeJSONError(w http.ResponseWriter, code int, message string) {
	body, _ := json.Marshal(map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    "proxy_error",
			"code":    code,
		},
	})
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_, _ = w.Write(body)
}

func (s *Server) probeOnce(channel string, sample []byte, isSSELine bool) {
	s.probedMu.Lock()
	if s.probed[channel] {
		s.probedMu.Unlock()
		return
	}
	s.probed[channel] = true
	s.probedMu.Unlock()

	var missing []string
	if isSSELine {
		missing = patch.FindMissingSSELine(string(sample))
	} else {
		missing = patch.FindMissingJSON(sample)
	}
	if len(missing) == 0 {
		s.log.Printf("schema probe channel=%s: no critical fields missing", channel)
		return
	}
	if len(missing) > 12 {
		missing = append(missing[:12], fmt.Sprintf("...(+%d more)", len(missing)-12))
	}
	s.log.Printf("schema probe channel=%s: filled %s", channel, strings.Join(missing, "; "))
}

func (s *Server) streamResponsesSSE(w http.ResponseWriter, response *http.Response, channel string, request facadeRequest, options patch.Options, started time.Time) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSONError(w, http.StatusInternalServerError, "stream unsupported")
		return
	}
	copySafeResponseHeaders(w.Header(), response.Header)
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(response.StatusCode)
	flusher.Flush()

	scanner := bufio.NewScanner(response.Body)
	scanner.Buffer(make([]byte, 64*1024), 16<<20)
	events := 0
	terminal := ""
	probed := false
	evidence := newSearchEvidence()
	clientWriteFailed := false
	writeFrame := func(lines []string) error {
		if len(lines) == 0 {
			return nil
		}
		payload, hasData := sseFramePayload(lines)
		trimmedPayload := strings.TrimSpace(payload)
		patchedData := ""
		if hasData {
			patchedData = "data: " + payload
		}
		if hasData && trimmedPayload != "" && trimmedPayload != "[DONE]" {
			restored, err := restoreClientWebSearchAliasJSON([]byte(payload), request.ClientSearchAlias)
			if err != nil {
				return fmt.Errorf("invalid upstream Responses SSE data: %w", err)
			}
			payload = string(restored)
			s.replays.captureJSON(channel, restored)
			evidence.observeJSON(restored)
			terminal = responseEventTerminal(payload, terminal)
			if terminal != "" && !probed {
				s.probeOnce(channel, []byte("data: "+payload), true)
				probed = true
			}
			patchedData = patch.PatchSSEDataLineWithSequence("data: "+payload, options, events)
			events++
		}
		insertedData := false
		for _, line := range lines {
			if strings.HasPrefix(line, "data:") {
				if !insertedData {
					if _, err := io.WriteString(w, patchedData+"\n"); err != nil {
						clientWriteFailed = true
						return err
					}
					insertedData = true
				}
				continue
			}
			if _, err := io.WriteString(w, line+"\n"); err != nil {
				clientWriteFailed = true
				return err
			}
		}
		if _, err := io.WriteString(w, "\n"); err != nil {
			clientWriteFailed = true
			return err
		}
		flusher.Flush()
		return nil
	}

	frame := make([]string, 0, 4)
	frameBytes := 0
	var streamErr error
	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if line == "" {
			if err := writeFrame(frame); err != nil {
				streamErr = err
				break
			}
			frame = frame[:0]
			frameBytes = 0
			continue
		}
		if frameBytes+len(line)+1 > maxSSEEventBytes {
			streamErr = fmt.Errorf("SSE event exceeds 16 MiB")
			break
		}
		frame = append(frame, line)
		frameBytes += len(line) + 1
	}
	if streamErr == nil {
		streamErr = scanner.Err()
	}
	if streamErr == nil && len(frame) > 0 {
		streamErr = writeFrame(frame)
	}
	if streamErr != nil {
		s.log.Printf("UP channel=%s SSE read error: %v", channel, streamErr)
		if !clientWriteFailed {
			writeResponsesStreamError(w, flusher, events, "upstream Responses stream failed")
		}
	}
	s.log.Printf("UP channel=%s SSE done events=%d terminal=%s %s", channel, events, terminal, time.Since(started).Round(time.Millisecond))
	s.logSearchEvidence(channel, request, evidence)
	if terminal == "" {
		s.log.Printf("UP channel=%s SSE ended without a Responses terminal event", channel)
		if streamErr == nil {
			writeResponsesStreamError(w, flusher, events, "upstream Responses stream ended without a terminal event")
		}
	}
}

func writeResponsesStreamError(w http.ResponseWriter, flusher http.Flusher, sequence int, message string) {
	payload, err := json.Marshal(map[string]any{
		"type":            "error",
		"code":            "proxy_stream_error",
		"message":         message,
		"param":           nil,
		"sequence_number": sequence,
	})
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(w, "event: error\ndata: %s\n\n", payload)
	flusher.Flush()
}

func sseFramePayload(lines []string) (string, bool) {
	var parts []string
	for _, line := range lines {
		if strings.HasPrefix(line, "data:") {
			value := strings.TrimPrefix(line, "data:")
			value = strings.TrimPrefix(value, " ")
			parts = append(parts, value)
		}
	}
	if len(parts) == 0 {
		return "", false
	}
	return strings.Join(parts, "\n"), true
}

func responseEventTerminal(payload, current string) string {
	var event map[string]any
	if json.Unmarshal([]byte(payload), &event) != nil {
		return current
	}
	switch event["type"] {
	case "response.completed":
		return "response.completed"
	case "response.incomplete":
		return "response.incomplete"
	case "response.failed":
		return "response.failed"
	default:
		return current
	}
}

func extractModel(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var request struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(body, &request) == nil {
		return request.Model
	}
	return ""
}
