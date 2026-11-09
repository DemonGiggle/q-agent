// Deterministic local fixtures for the credential-free Streamable HTTP
// transport (issue #48). Every test runs against httptest servers on
// loopback: no external network, no credentials, no shared state between
// tasks. Fixtures cover both SDK response modes (plain JSON and
// request-scoped SSE), required request metadata, and the predictable
// termination of malformed/oversized/401/403/disconnect/cancel paths with
// sanitized errors.
package mcpclient

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"q-agent/internal/config"
)

// recordedRequest is one observed server-side request: method, path, and a
// clone of the headers. Bodies are never retained.
type recordedRequest struct {
	method string
	path   string
	header http.Header
}

// recordingHandler wraps an SDK Streamable HTTP handler, capturing every
// request's metadata for credential and protocol assertions.
type recordingHandler struct {
	inner *mcp.StreamableHTTPHandler

	mu       sync.Mutex
	requests []recordedRequest
}

func (h *recordingHandler) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	h.mu.Lock()
	h.requests = append(h.requests, recordedRequest{
		method: req.Method,
		path:   req.URL.EscapedPath(),
		header: req.Header.Clone(),
	})
	h.mu.Unlock()
	h.inner.ServeHTTP(w, req)
}

func (h *recordingHandler) snapshot() []recordedRequest {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]recordedRequest(nil), h.requests...)
}

// httpTestServer starts a real SDK Streamable HTTP server on loopback with
// one greet tool, wrapped in request recording. opts selects the response
// mode (JSON vs request-scoped SSE) and session behavior.
func httpTestServer(t *testing.T, opts *mcp.StreamableHTTPOptions) (*httptest.Server, *recordingHandler) {
	t.Helper()
	srv := mcp.NewServer(testServerImpl, nil)
	mcp.AddTool(srv, &mcp.Tool{Name: "greet", Description: "say hi"}, sayHiForTest)
	rec := &recordingHandler{
		inner: mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, opts),
	}
	server := httptest.NewServer(rec)
	t.Cleanup(server.Close)
	return server, rec
}

// httpTestConfig builds one enabled http server entry pointed at the
// fixture with short timeouts to keep failure tests fast.
func httpTestConfig(endpoint string) map[string]config.MCPServer {
	return map[string]config.MCPServer{
		"http": {
			Enabled:        true,
			Transport:      "http",
			URL:            endpoint,
			ConnectTimeout: "5s",
			CallTimeout:    "5s",
		},
	}
}

// requireAvailable fails the test unless the task negotiated a live,
// versioned session for the fixture server.
func requireAvailable(t *testing.T, task *Task) {
	t.Helper()
	if task == nil {
		t.Fatal("StartTask returned nil task")
	}
	if !task.Available("http") {
		status, _ := task.Status("http")
		t.Fatalf("http server unavailable: %q", status.Error)
	}
	status, ok := task.Status("http")
	if !ok {
		t.Fatal("missing status for http server")
	}
	if status.ProtocolVersion == "" {
		t.Fatal("negotiated protocol version is empty")
	}
	supported := false
	for _, v := range SupportedProtocolVersions() {
		if v == status.ProtocolVersion {
			supported = true
			break
		}
	}
	if !supported {
		t.Fatalf("negotiated protocol version %q is not SDK-supported", status.ProtocolVersion)
	}
	if status.Transport != "http" {
		t.Fatalf("status transport = %q, want http", status.Transport)
	}
}

// requireNoCredentials fails the test if any observed request carried auth
// material or strayed from the fixture endpoint path.
func requireNoCredentials(t *testing.T, requests []recordedRequest) {
	t.Helper()
	if len(requests) == 0 {
		t.Fatal("no requests observed")
	}
	for i, req := range requests {
		for _, header := range []string{"Authorization", "Proxy-Authorization", "Cookie", "Cookie2"} {
			if value := req.header.Get(header); value != "" {
				t.Fatalf("request %d (%s %s) carried %s header", i, req.method, req.path, header)
			}
		}
		if req.path != "/" {
			t.Fatalf("request %d (%s %s) left the endpoint path: possible login flow", i, req.method, req.path)
		}
	}
}

// requireMetadata fails the test unless the observed requests carry the
// required Streamable HTTP metadata: JSON content type, dual accept, and a
// negotiated protocol version on tool calls.
func requireMetadata(t *testing.T, requests []recordedRequest) {
	t.Helper()
	var sawPost, sawVersion bool
	for _, req := range requests {
		if req.method != http.MethodPost {
			continue
		}
		sawPost = true
		accept := req.header.Get("Accept")
		if !strings.Contains(accept, "application/json") || !strings.Contains(accept, "text/event-stream") {
			t.Fatalf("POST Accept = %q, want application/json and text/event-stream", accept)
		}
		if contentType := req.header.Get("Content-Type"); !strings.Contains(contentType, "application/json") {
			t.Fatalf("POST Content-Type = %q, want application/json", contentType)
		}
		if version := req.header.Get("Mcp-Protocol-Version"); version != "" {
			sawVersion = true
		}
	}
	if !sawPost {
		t.Fatal("no POST requests observed")
	}
	if !sawVersion {
		t.Fatal("no request carried an Mcp-Protocol-Version header")
	}
}

// callGreet performs one bounded tool call over the task's live session and
// returns the rendered text of the first content item.
func callGreet(t *testing.T, task *Task, name string) string {
	t.Helper()
	session, ok := task.Session("http")
	if !ok {
		t.Fatal("no live session for http server")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "greet",
		Arguments: map[string]any{"name": name},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if result == nil || len(result.Content) == 0 {
		t.Fatal("CallTool returned no content")
	}
	text, ok := result.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("CallTool content type = %T, want *mcp.TextContent", result.Content[0])
	}
	return text.Text
}

func TestHTTPTransportJSONRoundTrip(t *testing.T) {
	server, rec := httpTestServer(t, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	task := StartTask(ctx, httpTestConfig(server.URL), HttpTransportFactory)
	defer task.Close()
	requireAvailable(t, task)

	if got := callGreet(t, task, "json"); got != "hi json" {
		t.Fatalf("CallTool text = %q, want %q", got, "hi json")
	}
	requireMetadata(t, rec.snapshot())
	requireNoCredentials(t, rec.snapshot())

	if err := task.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := task.Close(); err != nil {
		t.Fatalf("second Close is not idempotent: %v", err)
	}
	if task.Available("http") {
		t.Fatal("session still available after Close")
	}
}

func TestHTTPTransportSSERoundTrip(t *testing.T) {
	// No JSONResponse: the SDK server answers POSTs with request-scoped
	// text/event-stream bodies, exercising the SSE response path.
	server, rec := httpTestServer(t, &mcp.StreamableHTTPOptions{Stateless: true})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	task := StartTask(ctx, httpTestConfig(server.URL), HttpTransportFactory)
	defer task.Close()
	requireAvailable(t, task)

	if got := callGreet(t, task, "sse"); got != "hi sse" {
		t.Fatalf("CallTool text = %q, want %q", got, "hi sse")
	}
	requireMetadata(t, rec.snapshot())
	requireNoCredentials(t, rec.snapshot())
}

func TestHTTPTransportStatefulTeardown(t *testing.T) {
	// Stateful sessions get a session ID, so Close issues the best-effort
	// session DELETE before releasing idle connections.
	server, rec := httpTestServer(t, &mcp.StreamableHTTPOptions{})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	task := StartTask(ctx, httpTestConfig(server.URL), HttpTransportFactory)
	defer task.Close()
	requireAvailable(t, task)

	if got := callGreet(t, task, "state"); got != "hi state" {
		t.Fatalf("CallTool text = %q, want %q", got, "hi state")
	}
	if err := task.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	sawDelete := false
	for _, req := range rec.snapshot() {
		if req.method == http.MethodDelete {
			sawDelete = true
		}
	}
	if !sawDelete {
		t.Fatal("no DELETE observed during stateful teardown")
	}
	requireNoCredentials(t, rec.snapshot())
}

func TestHTTPTransportAuthFailures(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		wantSubstr string
	}{
		{name: "unauthorized", status: http.StatusUnauthorized, wantSubstr: "requires authentication"},
		{name: "forbidden", status: http.StatusForbidden, wantSubstr: "denied access"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var mu sync.Mutex
			var requests []recordedRequest
			fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				mu.Lock()
				requests = append(requests, recordedRequest{
					method: req.Method,
					path:   req.URL.EscapedPath(),
					header: req.Header.Clone(),
				})
				mu.Unlock()
				w.WriteHeader(test.status)
			}))
			t.Cleanup(fixture.Close)

			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			task := StartTask(ctx, httpTestConfig(fixture.URL), HttpTransportFactory)
			defer task.Close()
			if task.Available("http") {
				t.Fatal("server unexpectedly available")
			}
			status, ok := task.Status("http")
			if !ok {
				t.Fatal("missing status for http server")
			}
			if !strings.Contains(status.Error, test.wantSubstr) {
				t.Fatalf("status error = %q, want substring %q", status.Error, test.wantSubstr)
			}
			if !strings.Contains(status.Error, "no credentials") {
				t.Fatalf("status error = %q, want credential-free hint", status.Error)
			}
			if strings.Contains(status.Error, fixture.URL) {
				t.Fatalf("status error echoes endpoint: %q", status.Error)
			}
			mu.Lock()
			observed := append([]recordedRequest(nil), requests...)
			mu.Unlock()
			requireNoCredentials(t, observed)
		})
	}
}

func TestHTTPTransportRejectsPolicy(t *testing.T) {
	// None of these entries may cause a dial: the factory rejects them
	// before any byte is sent. closedURL proves it: nothing listens there,
	// so a dial attempt would fail differently (refused, not policy).
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve loopback port: %v", err)
	}
	closedURL := "http://" + ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("release loopback port: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*config.MCPServer)
		want   string
	}{
		{name: "wrong transport", mutate: func(s *config.MCPServer) { s.Transport = "stdio" }, want: "cannot dial"},
		{name: "empty url", mutate: func(s *config.MCPServer) { s.URL = "" }, want: "url is required"},
		{name: "command set", mutate: func(s *config.MCPServer) { s.Command = "x" }, want: "must not set command"},
		{name: "credential url", mutate: func(s *config.MCPServer) { s.URL = "http://user:pass@127.0.0.1:1/" }, want: "must not contain credentials"},
		{name: "remote plain http", mutate: func(s *config.MCPServer) { s.URL = "http://203.0.2.1/" }, want: "only allowed for loopback"},
		{name: "non http scheme", mutate: func(s *config.MCPServer) { s.URL = "ftp://127.0.0.1:1/" }, want: "want http(s) URL"},
		{name: "no host", mutate: func(s *config.MCPServer) { s.URL = "https:///path" }, want: "want http(s) URL"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := config.MCPServer{
				Enabled:        true,
				Transport:      "http",
				URL:            closedURL,
				ConnectTimeout: "2s",
				CallTimeout:    "2s",
			}
			test.mutate(&server)
			if _, err := HttpTransportFactory(context.Background(), "http", server); err == nil {
				t.Fatal("factory succeeded, want policy rejection")
			} else if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("factory error = %q, want substring %q", err.Error(), test.want)
			} else if strings.Contains(err.Error(), "pass") {
				t.Fatalf("factory error echoes credentials: %q", err.Error())
			}
		})
	}
}

func TestHTTPTransportEndpointPolicy(t *testing.T) {
	tests := []struct {
		raw     string
		wantErr bool
	}{
		{raw: "https://example.com/mcp", wantErr: false},
		{raw: "http://127.0.0.1:8080/mcp", wantErr: false},
		{raw: "http://127.0.0.5/mcp", wantErr: false},
		{raw: "http://[::1]:8080/mcp", wantErr: false},
		{raw: "http://localhost:8080/mcp", wantErr: false},
		{raw: "http://LOCALHOST:8080/mcp", wantErr: false},
		{raw: "http://example.com/mcp", wantErr: true},
		{raw: "http://10.0.0.1/mcp", wantErr: true},
		{raw: "http://user@example.com/mcp", wantErr: true},
		{raw: "https://user@example.com/mcp", wantErr: true},
		{raw: "ftp://127.0.0.1/mcp", wantErr: true},
		{raw: "http:///mcp", wantErr: true},
		{raw: "not a url", wantErr: true},
		{raw: "", wantErr: true},
	}
	for _, test := range tests {
		if err := checkHTTPEndpoint(test.raw); (err != nil) != test.wantErr {
			t.Errorf("checkHTTPEndpoint(%q) err = %v, wantErr = %v", test.raw, err, test.wantErr)
		}
	}
}

func TestHTTPTransportMalformed(t *testing.T) {
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("this is not json-rpc"))
	}))
	t.Cleanup(fixture.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	task := StartTask(ctx, httpTestConfig(fixture.URL), HttpTransportFactory)
	defer task.Close()
	if task.Available("http") {
		t.Fatal("malformed server unexpectedly available")
	}
	status, _ := task.Status("http")
	if status.Error == "" {
		t.Fatal("malformed server has empty status error")
	}
	if strings.Contains(status.Error, "\n") {
		t.Fatalf("status error is not one line: %q", status.Error)
	}
}

func TestHTTPTransportOversized(t *testing.T) {
	// A body far past MaxHTTPResponseBytes, streamed in chunks so a client
	// that stops reading never wedges the fixture.
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		chunk := make([]byte, 64<<10)
		for i := 0; i < MaxHTTPResponseBytes/(64<<10)+4; i++ {
			select {
			case <-req.Context().Done():
				return
			default:
			}
			if _, err := w.Write(chunk); err != nil {
				return
			}
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
		}
	}))
	t.Cleanup(fixture.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	task := StartTask(ctx, httpTestConfig(fixture.URL), HttpTransportFactory)
	defer task.Close()
	if task.Available("http") {
		t.Fatal("oversized server unexpectedly available")
	}
	status, _ := task.Status("http")
	if status.Error == "" {
		t.Fatal("oversized server has empty status error")
	}
}

func TestHTTPTransportDisconnect(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve loopback port: %v", err)
	}
	deadURL := "http://" + ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("release loopback port: %v", err)
	}

	servers := map[string]config.MCPServer{
		"http": {
			Enabled:        true,
			Transport:      "http",
			URL:            deadURL,
			ConnectTimeout: "5s",
			CallTimeout:    "5s",
		},
	}
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	task := StartTask(ctx, servers, HttpTransportFactory)
	defer task.Close()
	if elapsed := time.Since(start); elapsed > 15*time.Second {
		t.Fatalf("disconnect took %v, want fast failure", elapsed)
	}
	if task.Available("http") {
		t.Fatal("dead server unexpectedly available")
	}
	status, _ := task.Status("http")
	if status.Error == "" {
		t.Fatal("dead server has empty status error")
	}
}

func TestHTTPTransportCancel(t *testing.T) {
	var mu sync.Mutex
	var requests []recordedRequest
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		mu.Lock()
		requests = append(requests, recordedRequest{
			method: req.Method,
			path:   req.URL.EscapedPath(),
			header: req.Header.Clone(),
		})
		mu.Unlock()
		// Hang until the client goes away or a bound elapses, then answer.
		// The SDK detaches setup request contexts for session longevity,
		// so cancellation alone may not end the request: the bound does,
		// and connect_timeout still bounds setup in connectOne.
		select {
		case <-req.Context().Done():
		case <-time.After(2 * time.Second):
		}
		w.WriteHeader(http.StatusBadRequest)
	}))
	t.Cleanup(fixture.Close)

	parent, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	task := StartTask(parent, httpTestConfig(fixture.URL), HttpTransportFactory)
	defer task.Close()
	if elapsed := time.Since(start); elapsed > 15*time.Second {
		t.Fatalf("cancelled setup took %v, want a bounded return", elapsed)
	}
	if task.Available("http") {
		t.Fatal("cancelled server unexpectedly available")
	}
	status, _ := task.Status("http")
	if status.Error == "" {
		t.Fatal("cancelled server has empty status error")
	}
	// The dial was attempted (no login flow elsewhere) and repeated Close
	// stays safe with no live session.
	mu.Lock()
	observed := append([]recordedRequest(nil), requests...)
	mu.Unlock()
	if len(observed) == 0 {
		t.Fatal("no requests observed")
	}
	requireNoCredentials(t, observed)
	if err := task.Close(); err != nil {
		t.Fatalf("Close after cancel: %v", err)
	}
}

func TestHTTPTransportRedirectPolicy(t *testing.T) {
	target, targetRec := httpTestServer(t, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})

	t.Run("loopback redirect succeeds without credentials", func(t *testing.T) {
		front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			http.Redirect(w, req, target.URL, http.StatusTemporaryRedirect)
		}))
		t.Cleanup(front.Close)

		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		task := StartTask(ctx, httpTestConfig(front.URL), HttpTransportFactory)
		defer task.Close()
		requireAvailable(t, task)
		if got := callGreet(t, task, "redirect"); got != "hi redirect" {
			t.Fatalf("CallTool text = %q, want %q", got, "hi redirect")
		}
		requireNoCredentials(t, targetRec.snapshot())
	})

	t.Run("redirect off loopback is rejected", func(t *testing.T) {
		front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			// TEST-NET-3 is never loopback; policy rejects before dialing,
			// so no packet leaves the process.
			http.Redirect(w, req, "http://203.0.2.1/", http.StatusTemporaryRedirect)
		}))
		t.Cleanup(front.Close)

		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		task := StartTask(ctx, httpTestConfig(front.URL), HttpTransportFactory)
		defer task.Close()
		if task.Available("http") {
			t.Fatal("off-loopback redirect unexpectedly available")
		}
		status, _ := task.Status("http")
		if !strings.Contains(status.Error, "loopback") {
			t.Fatalf("status error = %q, want loopback policy rejection", status.Error)
		}
	})

	t.Run("redirect to credentials is rejected", func(t *testing.T) {
		front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			http.Redirect(w, req, "http://user:pass@127.0.0.1:1/", http.StatusTemporaryRedirect)
		}))
		t.Cleanup(front.Close)

		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		task := StartTask(ctx, httpTestConfig(front.URL), HttpTransportFactory)
		defer task.Close()
		if task.Available("http") {
			t.Fatal("credential redirect unexpectedly available")
		}
		status, _ := task.Status("http")
		if !strings.Contains(status.Error, "credentials") {
			t.Fatalf("status error = %q, want credential rejection", status.Error)
		}
	})
}

func TestBoundedBody(t *testing.T) {
	newBody := func(content string, limit int64) *boundedBody {
		return &boundedBody{rc: io.NopCloser(strings.NewReader(content)), left: limit, max: limit}
	}

	t.Run("within limit reads through", func(t *testing.T) {
		body := newBody("hello", 16)
		buf := make([]byte, 16)
		n, err := body.Read(buf)
		if err != nil && n != len("hello") {
			t.Fatalf("Read = %d, %v", n, err)
		}
		if string(buf[:n]) != "hello" {
			t.Fatalf("Read = %q", buf[:n])
		}
		if err := body.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	})

	t.Run("past limit fails instead of truncating", func(t *testing.T) {
		body := newBody(strings.Repeat("x", 32), 8)
		buf := make([]byte, 64)
		if _, err := body.Read(buf); err == nil {
			t.Fatal("Read succeeded past the limit")
		} else if !strings.Contains(err.Error(), "exceeds 8 bytes") {
			t.Fatalf("Read error = %q, want size bound", err.Error())
		}
		if _, err := body.Read(buf); err == nil {
			t.Fatal("second Read succeeded after overflow")
		}
	})

	t.Run("short reads accumulate toward the limit", func(t *testing.T) {
		body := newBody(strings.Repeat("y", 10), 8)
		small := make([]byte, 4)
		if _, err := body.Read(small); err != nil {
			t.Fatalf("first Read: %v", err)
		}
		if _, err := body.Read(small); err != nil {
			t.Fatalf("second Read: %v", err)
		}
		if _, err := body.Read(small); err == nil {
			t.Fatal("third Read succeeded past the limit")
		}
	})
}

func TestClarifyHTTPError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{name: "unauthorized", err: fmt.Errorf(`sending "initialize": Unauthorized`), want: "requires authentication (HTTP 401)"},
		{name: "status 401", err: fmt.Errorf("POST failed: 401"), want: "requires authentication (HTTP 401)"},
		{name: "forbidden", err: fmt.Errorf(`sending "tools/call": Forbidden`), want: "denied access (HTTP 403)"},
		{name: "status 403", err: fmt.Errorf("POST failed: 403"), want: "denied access (HTTP 403)"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := clarifyHTTPError("http", test.err)
			if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("clarified error = %q, want %q", err.Error(), test.want)
			}
			if !strings.Contains(err.Error(), "never starts a login") {
				t.Fatalf("clarified error = %q, want login-flow hint", err.Error())
			}
		})
	}
	t.Run("other errors pass through", func(t *testing.T) {
		sentinel := fmt.Errorf("connection refused")
		if err := clarifyHTTPError("http", sentinel); err != sentinel {
			t.Fatalf("non-auth error was rewritten: %v", err)
		}
		if err := clarifyHTTPError("http", nil); err != nil {
			t.Fatalf("nil error became %v", err)
		}
	})
}
