// Credential-free Streamable HTTP transport for MCP servers (issue #48).
//
// This adapter dials one configured [mcp.servers.<name>] HTTP endpoint via
// the SDK's StreamableClientTransport, which carries both plain-JSON and
// request-scoped SSE responses and sends the required request metadata
// (Content-Type, Accept: application/json + text/event-stream, the
// negotiated MCP-Protocol-Version, and the session ID once assigned).
// Protocol version compatibility rides the SDK's server/discover flow with
// legacy initialize fallback in client.Connect; this file declares no
// versions of its own (see SupportedProtocolVersions).
//
// Credential policy (no auth material ever leaves the process):
//   - Endpoints are HTTPS anywhere, or plain HTTP on loopback only
//     (127.0.0.0/8, ::1, localhost). Anything else is rejected before any
//     byte is sent, as are URLs with embedded userinfo.
//   - The transport sends no Authorization/Cookie/Proxy-Authorization
//     headers: a scrubbing RoundTripper deletes them from every outgoing
//     request, no cookie jar is configured, and the SDK's OAuth handler is
//     left nil so 401/403 responses never start a login flow.
//   - Redirects are followed only when every hop satisfies the same
//     endpoint policy; a redirect to credentials, plain non-loopback HTTP,
//     or a non-http(s) scheme aborts the request.
//   - Legacy HTTP+SSE (the SSEClientTransport GET/event handshake) is out
//     of scope and never constructed here.
//
// Bounding and lifecycle:
//   - Per-event SSE buffering is capped (MaxHTTPEventBytes) and every
//     response body is capped in total (MaxHTTPResponseBytes): oversized or
//     malformed bodies fail the connection instead of exhausting memory.
//   - Setup is bounded by the per-server connect_timeout in connectOne;
//     per-call bounds apply in execution (#50). Cancellation propagates via
//     the request contexts (the SDK may detach setup request contexts for
//     session longevity, so connect_timeout still bounds setup when the
//     peer never answers); the SDK closes response bodies on every error
//     path and Close issues a best-effort session DELETE before releasing
//     idle connections. Close is idempotent.
//   - Calls are never replayed after an ambiguous failure: this adapter
//     performs no retries of its own (the SDK only resumes request-scoped
//     SSE streams within MaxRetries), and lifecycle.go marks unknown
//     outcomes via ErrAmbiguous/IsAmbiguous.
//   - The standalone background SSE stream is disabled: every HTTP exchange
//     is scoped to one request context, so no persistent stream can outlive
//     the task and teardown stays deterministic. Server-initiated traffic
//     still arrives on request-scoped SSE streams.
//
// The factory plugs into lifecycle.go's TransportFactory as-is: it returns
// exactly one SDK Transport per call, retains nothing after return, and the
// Task owns the resulting session lifetime.
package mcpclient

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"q-agent/internal/config"
)

const (
	// MaxHTTPEventBytes bounds one inbound SSE event while reading a
	// Streamable HTTP response. Servers emitting larger events fail the
	// connection instead of exhausting memory.
	MaxHTTPEventBytes = 4 << 20
	// MaxHTTPResponseBytes bounds the total bytes read from any single
	// Streamable HTTP response body (JSON or SSE). Oversized bodies fail
	// the request instead of exhausting memory.
	MaxHTTPResponseBytes = 4 << 20
	// maxHTTPRedirects caps followed redirects; redirect targets must
	// satisfy the same endpoint policy as the configured endpoint.
	maxHTTPRedirects = 10
)

// HttpTransportFactory dials one enabled [mcp.servers.<name>] HTTP endpoint
// over credential-free Streamable HTTP. It satisfies TransportFactory for
// use with StartTask: dial is called at most once per server per Task, and
// the returned transport is used for at most one Client.Connect.
//
// Dial failures are definitive (misconfiguration or policy rejection): the
// caller records them as unavailable without retry. Failures after Connect
// begins have unknown outcome and are wrapped by connectOne as ambiguous.
func HttpTransportFactory(ctx context.Context, name string, server config.MCPServer) (SDKTransport, error) {
	if err := checkHTTPServer(server); err != nil {
		return nil, err
	}
	endpoint := strings.TrimSpace(server.URL)
	client := credentialFreeClient()
	inner := &mcp.StreamableClientTransport{
		Endpoint:   endpoint,
		HTTPClient: client,
		// No OAuth handler, no custom headers, no cookie jar: 401/403
		// responses surface as errors; no login flow ever starts.
		MaxEventSize: MaxHTTPEventBytes,
		// Every exchange is scoped to its request context so buffering,
		// cancellation, and teardown stay deterministic per task; the
		// persistent standalone SSE stream would outlive that scope.
		DisableStandaloneSSE: true,
	}
	_ = ctx // setup bounds and cancellation flow through Connect/Write/Read.
	return &httpClientTransport{inner: inner, client: client, server: name}, nil
}

// checkHTTPServer validates one server entry for HTTP dialing without
// echoing host details. Load-time validation already enforces this shape;
// the checks keep direct struct literals usable and failures definitive.
func checkHTTPServer(server config.MCPServer) error {
	if server.Transport != "http" {
		return fmt.Errorf("mcp: http transport cannot dial %q transport", server.Transport)
	}
	if strings.TrimSpace(server.URL) == "" {
		return fmt.Errorf("mcp: http server url is required")
	}
	if server.Command != "" || len(server.Args) != 0 {
		return fmt.Errorf("mcp: http server must not set command or args")
	}
	if err := checkHTTPEndpoint(server.URL); err != nil {
		return err
	}
	return nil
}

// checkHTTPEndpoint enforces the credential-free endpoint policy: HTTPS
// anywhere, plain HTTP on loopback only, never with userinfo, http(s)
// scheme only. It reports sanitized errors that never echo the URL.
func checkHTTPEndpoint(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("mcp: http server url is invalid: want http(s) URL with a host")
	}
	if u.User != nil {
		return fmt.Errorf("mcp: http server url must not contain credentials")
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if isHTTPLoopbackHost(u.Hostname()) {
			return nil
		}
		return fmt.Errorf("mcp: http server url: plain http is only allowed for loopback (127.0.0.0/8, ::1, localhost); use https")
	default:
		return fmt.Errorf("mcp: http server url is invalid: want http(s) URL with a host")
	}
}

// isHTTPLoopbackHost reports whether host is a loopback address for the
// plain-http exception: 127.0.0.0/8, ::1, or localhost. It mirrors the
// load-time check in internal/config so direct struct literals get the same
// policy without importing unexported helpers.
func isHTTPLoopbackHost(host string) bool {
	if strings.EqualFold(strings.TrimSpace(host), "localhost") {
		return true
	}
	if ip := net.ParseIP(strings.TrimSpace(host)); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// credentialFreeClient builds a per-server HTTP client that never sends or
// stores credentials: no cookie jar, header scrubbing plus a total response
// body cap on every exchange, and redirect handling consistent with the
// endpoint policy. A fresh client per factory call keeps tasks from sharing
// mutable transport state.
func credentialFreeClient() *http.Client {
	base, ok := http.DefaultTransport.(*http.Transport)
	var transport *http.Transport
	if ok && base != nil {
		transport = base.Clone()
	} else {
		transport = &http.Transport{}
	}
	// No Jar: without a cookie jar no Set-Cookie is stored and no Cookie
	// header is ever synthesized.
	return &http.Client{
		Transport: &httpPolicyTripper{
			base:    transport,
			maxBody: MaxHTTPResponseBytes,
		},
		CheckRedirect: httpPolicyRedirect,
	}
}

// httpPolicyRedirect follows redirects only while every hop satisfies the
// endpoint policy, within maxHTTPRedirects hops. Violations abort the
// request before any credential or plaintext leak can occur.
func httpPolicyRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= maxHTTPRedirects {
		return fmt.Errorf("mcp: http server redirected too many times")
	}
	if req == nil || req.URL == nil {
		return fmt.Errorf("mcp: http server redirect is invalid")
	}
	if err := checkHTTPEndpoint(req.URL.String()); err != nil {
		return err
	}
	return nil
}

// httpPolicyTripper scrubs credential headers from every outgoing request
// and caps the total bytes read from every response body. It never adds
// headers; the SDK sets the required MCP metadata itself.
type httpPolicyTripper struct {
	base    http.RoundTripper
	maxBody int64
}

func (rt *httpPolicyTripper) baseRoundTripper() http.RoundTripper {
	if rt == nil || rt.base == nil {
		return http.DefaultTransport
	}
	return rt.base
}

func (rt *httpPolicyTripper) maxResponseBody() int64 {
	if rt == nil || rt.maxBody <= 0 {
		return MaxHTTPResponseBytes
	}
	return rt.maxBody
}

// RoundTrip implements http.RoundTripper.
func (rt *httpPolicyTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	out := req
	if req != nil {
		// Clone before mutating: the SDK may reuse the request on retry.
		out = req.Clone(req.Context())
		out.Header.Del("Authorization")
		out.Header.Del("Proxy-Authorization")
		out.Header.Del("Cookie")
		out.Header.Del("Cookie2")
	}
	resp, err := rt.baseRoundTripper().RoundTrip(out)
	if err != nil {
		return nil, err
	}
	if resp != nil && resp.Body != nil {
		max := rt.maxResponseBody()
		resp.Body = &boundedBody{rc: resp.Body, left: max, max: max}
	}
	return resp, nil
}

// errHTTPResponseTooLarge reports a response body that exceeded the total
// byte cap. The message carries only the bound, never body content.
var errHTTPResponseTooLarge = fmt.Errorf("mcp: http response exceeds size limit")

// boundedBody caps the total bytes read from a response body. Reads past
// the cap fail instead of truncating silently, so oversized JSON/SSE bodies
// terminate predictably. Close always releases the underlying stream.
type boundedBody struct {
	rc   io.ReadCloser
	left int64
	max  int64
	err  error
}

func (b *boundedBody) Read(p []byte) (int, error) {
	if b == nil || b.rc == nil {
		return 0, io.EOF
	}
	if b.err != nil {
		return 0, b.err
	}
	// Permit one byte past the limit so overflow is detected as an error
	// rather than a silent truncation.
	allowance := b.left + 1
	if allowance < 1 {
		allowance = 1
	}
	if int64(len(p)) > allowance {
		p = p[:allowance]
	}
	if len(p) == 0 {
		return 0, nil
	}
	n, err := b.rc.Read(p)
	if int64(n) > b.left {
		b.err = fmt.Errorf("mcp: http response exceeds %d bytes", b.max)
		return 0, b.err
	}
	b.left -= int64(n)
	if err != nil {
		// Preserve EOF and transport errors (including cancellation);
		// callers observe the real terminal state.
		b.err = err
	}
	return n, err
}

func (b *boundedBody) Close() error {
	if b == nil || b.rc == nil {
		return nil
	}
	return b.rc.Close()
}

// httpClientTransport adapts the SDK Streamable HTTP transport with
// credential-free error surfacing: 401/403 failures read as explicit
// authentication/access failures (no login is ever initiated), and Close
// additionally releases idle connections so no task resources leak. It
// retains nothing after return; the Task owns the session lifetime.
type httpClientTransport struct {
	inner  *mcp.StreamableClientTransport
	client *http.Client
	server string

	closeOnce sync.Once
}

var _ SDKTransport = (*httpClientTransport)(nil)

// Connect implements mcp.Transport.
func (t *httpClientTransport) Connect(ctx context.Context) (mcp.Connection, error) {
	if t == nil || t.inner == nil {
		return nil, fmt.Errorf("mcp: http transport is not configured")
	}
	conn, err := t.inner.Connect(ctx)
	if err != nil {
		return nil, clarifyHTTPError(t.server, err)
	}
	if conn == nil {
		return nil, fmt.Errorf("mcp: http transport connected with nil connection")
	}
	return &httpClientConn{inner: conn, server: t.server, client: t.client}, nil
}

// httpClientConn scopes error surfacing and cleanup to one connection.
// Reads and writes propagate the caller's cancellation via ctx; Close is
// idempotent and also drains the per-server client's idle connections.
type httpClientConn struct {
	inner  mcp.Connection
	server string
	client *http.Client

	closeOnce sync.Once
	closeErr  error
}

var _ mcp.Connection = (*httpClientConn)(nil)

// Read implements mcp.Connection.
func (c *httpClientConn) Read(ctx context.Context) (jsonrpc.Message, error) {
	msg, err := c.inner.Read(ctx)
	if err != nil {
		return nil, clarifyHTTPError(c.server, err)
	}
	return msg, nil
}

// Write implements mcp.Connection.
func (c *httpClientConn) Write(ctx context.Context, msg jsonrpc.Message) error {
	if err := c.inner.Write(ctx, msg); err != nil {
		return clarifyHTTPError(c.server, err)
	}
	return nil
}

// Close implements mcp.Connection.
func (c *httpClientConn) Close() error {
	if c == nil {
		return nil
	}
	c.closeOnce.Do(func() {
		if c.inner != nil {
			c.closeErr = c.inner.Close()
		}
		// Release keep-alive connections owned by this server's client so
		// task teardown leaves no idle sockets behind. In-use streams are
		// unaffected; they close via the SDK connection teardown above.
		if c.client != nil {
			c.client.CloseIdleConnections()
		}
	})
	return c.closeErr
}

// SessionID implements mcp.Connection.
func (c *httpClientConn) SessionID() string {
	if c == nil || c.inner == nil {
		return ""
	}
	return c.inner.SessionID()
}

// clarifyHTTPError maps transport failures that carry HTTP 401/403 signals
// to explicit credential-free failures. The transport sends no credentials
// and the SDK OAuth handler is nil, so these errors never initiate a login
// flow; the hint says so. All other errors pass through untouched for the
// lifecycle's sanitized recording.
func clarifyHTTPError(server string, err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	lowered := strings.ToLower(msg)
	switch {
	case strings.Contains(lowered, "401") || strings.Contains(lowered, "unauthorized"):
		return fmt.Errorf("mcp: http server %s requires authentication (HTTP 401): this transport sends no credentials and never starts a login flow: %w", sanitizeName(server), err)
	case strings.Contains(lowered, "403") || strings.Contains(lowered, "forbidden"):
		return fmt.Errorf("mcp: http server %s denied access (HTTP 403): this transport sends no credentials and never starts a login flow: %w", sanitizeName(server), err)
	default:
		return err
	}
}
