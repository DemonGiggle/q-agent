// Package mcpclient lifecycle manages task-scoped MCP client sessions.
//
// One Task owns one connection lifetime: StartTask dials each enabled
// server over its transport adapter (#47 stdio, #48 Streamable HTTP),
// negotiates the protocol version via the SDK's server/discover flow with
// legacy initialize fallback, records sanitized per-server availability,
// and Close deterministically tears everything down.
//
// Design notes for transport adapters and the runtime wrapper (#51):
//   - Transports plug in via TransportFactory: given one server entry,
//     return exactly one SDK Transport for this task. The Task owns the
//     resulting ClientSession lifetime; adapters must not retain it.
//   - The runtime wrapper creates one Task per agent task via StartTask
//     and defers Close/CloseTask. Tasks never share mutable state between
//     chats: there are no package globals; each Task has its own Client
//     and session map.
//   - Setup is bounded per server (connect_timeout), propagates parent
//     cancellation, isolates failures (one bad server never disables
//     healthy servers or ordinary tools), and cleanup is idempotent and
//     safe for partial setup, cancellation, and repeated calls.
//   - Never replay a call after an ambiguous connection failure: use
//     IsAmbiguous to detect AmbiguousError/ErrAmbiguous from setup.
//     Execution (#50) must surface such errors without retry.
package mcpclient

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"q-agent/internal/config"
)

// Client identity advertised to servers.
const (
	// ClientName is the MCP client name sent in initialize/discover.
	ClientName = "q-agent"
	// ClientVersion is the client version string. Overridden at build
	// time when available; "dev" keeps local/test builds working.
	ClientVersion = "dev"
)

// NewClient returns a task-scoped SDK client that advertises only
// implemented capabilities.
//
// Roots, sampling, elicitation, and extensions are intentionally
// disabled: Capabilities is non-nil empty so the SDK default
// (roots with listChanged) is suppressed and no handlers infer
// sampling/elicitation. Do not add handlers here; future client-initiated
// features require an explicit capability decision.
func NewClient() *SDKClient {
	return mcp.NewClient(
		&mcp.Implementation{Name: ClientName, Version: ClientVersion},
		&mcp.ClientOptions{Capabilities: &mcp.ClientCapabilities{}},
	)
}

// SupportedProtocolVersions reports the SDK-supported protocol versions,
// newest first. The current MCP revision is 2026-07-28; earlier entries
// are the SDK-supported legacy revisions accepted via initialize fallback.
func SupportedProtocolVersions() []string {
	return mcp.SupportedProtocolVersions()
}

// TransportFactory supplies exactly one SDK transport for one named server
// for one task. Implementations are the stdio adapter (#47) and the
// Streamable HTTP adapter (#48).
//
// The factory is called at most once per server per Task. Returned
// transports are used for at most one Client.Connect, per SDK rules.
// Connection lifetime belongs to the Task: adapters must not retain the
// transport or session after return, and Transport.Connection.Close must
// be idempotent so Task.Close can deterministically release partial setup.
type TransportFactory func(ctx context.Context, name string, server config.MCPServer) (SDKTransport, error)

// ServerStatus is the sanitized per-server availability record for one task.
// It never carries command lines, arguments, URLs, environment, or other
// host details: only the server name, transport kind, negotiated version,
// capability summary, and a truncated error string.
type ServerStatus struct {
	// Name is the [mcp.servers.<name>] key.
	Name string
	// Transport is the configured transport kind ("stdio"|"http").
	Transport string
	// Available is true when setup negotiated a live session.
	Available bool
	// ProtocolVersion is the negotiated MCP version for available servers.
	ProtocolVersion string
	// ServerName/ServerVersion identify the remote implementation.
	ServerName    string
	ServerVersion string
	// HasTools/HasPrompts/HasResources summarize server capabilities.
	HasTools     bool
	HasPrompts   bool
	HasResources bool
	// Error is the sanitized setup failure for unavailable servers.
	Error string
}

// Task owns one agent task's MCP sessions: one Client plus at most one
// ClientSession per server. The zero value is unusable; create via StartTask.
// A Task is bound to the task that created it and must not be shared
// between chats or topics.
type Task struct {
	mu       sync.Mutex
	client   *SDKClient
	sessions map[string]*SDKClientSession
	statuses map[string]ServerStatus
	order    []string
	closed   bool
}

// StartTask dials every entry in servers with a bounded per-server setup
// timeout (connect_timeout), propagating parent cancellation.
//
// It blocks until every server has either connected or failed: healthy
// servers are usable even when others are unavailable or incompatible.
// Failures are isolated per server and recorded in Statuses; StartTask
// itself never fails. The caller owns the Task and must defer Close (or
// CloseTask) to end the connection lifetime. A later task reconnects by
// calling StartTask again; sessions are never reused across tasks.
//
// dial supplies the transport per server. A nil dial marks every server
// unavailable instead of panicking, keeping ordinary tools usable.
func StartTask(ctx context.Context, servers map[string]config.MCPServer, dial TransportFactory) *Task {
	names := make([]string, 0, len(servers))
	for name := range servers {
		names = append(names, name)
	}
	sort.Strings(names)

	t := &Task{
		client:   NewClient(),
		sessions: map[string]*SDKClientSession{},
		statuses: map[string]ServerStatus{},
		order:    names,
	}
	if len(names) == 0 {
		return t
	}

	var wg sync.WaitGroup
	for _, name := range names {
		server := servers[name]
		wg.Add(1)
		go func() {
			defer wg.Done()
			session, result, statusErr := connectOne(ctx, t.client, name, server, dial)
			t.mu.Lock()
			defer t.mu.Unlock()
			if t.closed {
				// Task was closed while setup was in flight (cancellation
				// path). Release the late session immediately so no
				// connection or goroutine leaks.
				if session != nil {
					_ = session.Close()
				}
				return
			}
			if session != nil && result != nil && statusErr == "" {
				t.sessions[name] = session
				t.statuses[name] = statusFromResult(name, server, result)
			} else {
				if session != nil {
					_ = session.Close()
				}
				t.statuses[name] = ServerStatus{
					Name:      name,
					Transport: server.Transport,
					Available: false,
					Error:     statusErr,
				}
			}
		}()
	}
	wg.Wait()
	// Fill any missing statuses defensively (should not happen: every
	// goroutine records exactly one status).
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, name := range names {
		if _, ok := t.statuses[name]; !ok {
			t.statuses[name] = ServerStatus{
				Name:      name,
				Transport: servers[name].Transport,
				Available: false,
				Error:     "mcp: setup did not report a status",
			}
		}
	}
	return t
}

// connectOne performs bounded setup for a single server: obtain its
// transport, then run SDK discovery (server/discover on modern servers)
// with legacy initialize fallback. The SDK performs protocol version
// handling for MCP 2026-07-28 and earlier SDK-supported revisions; this
// wrapper never assumes every server needs the init handshake.
//
// Returns a live session plus its InitializeResult on success. On failure
// returns a sanitized error string; transport-dial failures are definitive
// (unavailable), while post-dial Connect failures of unknown outcome are
// wrapped as ambiguous (see ErrAmbiguous) so callers never replay them.
func connectOne(parent context.Context, client *SDKClient, name string, server config.MCPServer, dial TransportFactory) (*SDKClientSession, *mcp.InitializeResult, string) {
	if parent.Err() != nil {
		return nil, nil, sanitizeError(parent.Err())
	}
	if dial == nil {
		return nil, nil, "mcp: no transport factory for " + sanitizeName(name)
	}
	timeout := ConnectTimeout(server)
	setupCtx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	transport, err := dial(setupCtx, name, server)
	if err != nil {
		if setupCtx.Err() != nil && parent.Err() == nil {
			// Dial exceeded the per-server bound without parent
			// cancellation: report the timeout, not the raw error.
			return nil, nil, sanitizeError(context.DeadlineExceeded)
		}
		return nil, nil, sanitizeError(err)
	}
	if transport == nil {
		return nil, nil, "mcp: transport factory returned nil transport"
	}

	session, err := client.Connect(setupCtx, transport, nil)
	if err != nil {
		if setupCtx.Err() != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
			return nil, nil, sanitizeError(err)
		}
		if isVersionMismatch(err) {
			return nil, nil, sanitizeError(err)
		}
		return nil, nil, sanitizeError(NewAmbiguousError(name, "connect", err))
	}
	if session == nil {
		return nil, nil, sanitizeError(NewAmbiguousError(name, "connect", errors.New("mcp: client connected with nil session")))
	}
	result := session.InitializeResult()
	if result == nil {
		_ = session.Close()
		return nil, nil, sanitizeError(NewAmbiguousError(name, "connect", errors.New("mcp: missing initialize result")))
	}
	if result.ProtocolVersion == "" {
		_ = session.Close()
		return nil, nil, sanitizeError(NewAmbiguousError(name, "connect", errors.New("mcp: empty negotiated protocol version")))
	}
	return session, result, ""
}

// statusFromResult builds the sanitized availability record for a live
// session. Capability booleans use the SDK capability APIs; only tool,
// prompt, and resource support is summarized here.
func statusFromResult(name string, server config.MCPServer, result *mcp.InitializeResult) ServerStatus {
	status := ServerStatus{
		Name:            name,
		Transport:       server.Transport,
		Available:       true,
		ProtocolVersion: result.ProtocolVersion,
	}
	if result.ServerInfo != nil {
		status.ServerName = result.ServerInfo.Name
		status.ServerVersion = result.ServerInfo.Version
	}
	if caps := result.Capabilities; caps != nil {
		status.HasTools = caps.Tools != nil
		status.HasPrompts = caps.Prompts != nil
		status.HasResources = caps.Resources != nil
	}
	return status
}

// Session returns the live session for an available server. The session is
// owned by the Task: callers must not Close it; Task.Close ends the
// connection lifetime. The second result is false for unknown or
// unavailable servers, keeping healthy servers and ordinary tools usable.
func (t *Task) Session(name string) (*SDKClientSession, bool) {
	if t == nil {
		return nil, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	session, ok := t.sessions[name]
	return session, ok && session != nil && !t.closed
}

// Status returns the sanitized availability record for one server.
func (t *Task) Status(name string) (ServerStatus, bool) {
	if t == nil {
		return ServerStatus{}, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	status, ok := t.statuses[name]
	return status, ok
}

// Statuses returns a snapshot of all per-server records in sorted name
// order. Unavailable servers are included with their sanitized errors;
// callers surface those as ordinary diagnostics, never as fatal errors
// that would disable healthy servers.
func (t *Task) Statuses() []ServerStatus {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]ServerStatus, 0, len(t.order))
	for _, name := range t.order {
		if status, ok := t.statuses[name]; ok {
			out = append(out, status)
		}
	}
	return out
}

// Available reports whether the named server has a live session in this task.
func (t *Task) Available(name string) bool {
	_, ok := t.Session(name)
	return ok
}

// Names returns the sorted server names attempted by this task.
func (t *Task) Names() []string {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]string(nil), t.order...)
}

// Close ends the task's connection lifetime, closing every session.
// It is deterministic, concurrency-safe, and idempotent: repeated calls
// return nil, partial setup (no sessions) is a no-op, and cancellation
// during setup still releases sessions that completed before Close.
// Close errors are joined; the Task is marked closed even on error.
func (t *Task) Close() error {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil
	}
	t.closed = true
	sessions := make([]*SDKClientSession, 0, len(t.sessions))
	for _, session := range t.sessions {
		if session != nil {
			sessions = append(sessions, session)
		}
	}
	t.sessions = map[string]*SDKClientSession{}
	t.mu.Unlock()

	var errs []error
	for _, session := range sessions {
		if err := session.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// CloseTask ends the connection lifetime for agent.Run's
// StartTask/CloseTask hook pair (defer scoped.CloseTask()). It never
// returns an error; setup failures are already recorded in Statuses.
func (t *Task) CloseTask() {
	_ = t.Close()
}

// ErrAmbiguous marks a connection failure whose outcome is unknown: the
// request may or may not have reached the server. Callers must surface
// such errors without replaying the call, or a later task may duplicate a
// side effect. One task is one connection lifetime; reconnect with a new
// Task instead of retrying on the same session.
var ErrAmbiguous = errors.New("mcp: ambiguous connection failure; do not replay")

// AmbiguousError wraps a setup or transport failure of unknown outcome for
// one server and operation. Use IsAmbiguous to detect it.
type AmbiguousError struct {
	// Server is the [mcp.servers.<name>] key.
	Server string
	// Op is the failing operation (for example "connect").
	Op string
	// Err is the underlying failure.
	Err error
}

// NewAmbiguousError marks err as ambiguous for server/op. A nil err
// yields a generic failure so the marker is never empty.
func NewAmbiguousError(server, op string, err error) *AmbiguousError {
	if err == nil {
		err = errors.New("mcp: unknown connection failure")
	}
	return &AmbiguousError{Server: server, Op: op, Err: err}
}

func (e *AmbiguousError) Error() string {
	if e == nil {
		return ErrAmbiguous.Error()
	}
	server, op := sanitizeName(e.Server), e.Op
	if op == "" {
		op = "operation"
	}
	if e.Err == nil {
		return "mcp: ambiguous " + op + " failure for server " + server + "; do not replay"
	}
	return "mcp: ambiguous " + op + " failure for server " + server + "; do not replay: " + e.Err.Error()
}

// Unwrap returns the underlying failure.
func (e *AmbiguousError) Unwrap() error {
	if e == nil || e.Err == nil {
		return ErrAmbiguous
	}
	return e.Err
}

// Is reports ErrAmbiguous so errors.Is(err, ErrAmbiguous) detects it
// through wrapping.
func (e *AmbiguousError) Is(target error) bool {
	return target == ErrAmbiguous
}

// IsAmbiguous reports whether err must not be replayed because the
// connection failed at an ambiguous point.
func IsAmbiguous(err error) bool {
	return errors.Is(err, ErrAmbiguous)
}

// IsConnectionClosed reports whether err wraps the SDK's closed-connection
// signal. Closed connections end the task lifetime; reconnect with a new
// Task instead of replaying.
func IsConnectionClosed(err error) bool {
	return errors.Is(err, mcp.ErrConnectionClosed)
}

// isVersionMismatch reports definitive protocol incompatibility: the
// server answered with an unsupported-version signal. Such failures are
// not ambiguous and must be surfaced clearly without retry on the same
// Task.
func isVersionMismatch(err error) bool {
	if err == nil {
		return false
	}
	var wire *jsonrpc.Error
	if errors.As(err, &wire) && wire.Code == mcp.CodeUnsupportedProtocolVersion {
		return true
	}
	lowered := strings.ToLower(err.Error())
	return strings.Contains(lowered, "unsupported protocol version")
}

// sanitizeError truncates failure text for status records. Transport and
// protocol errors are ordinary diagnostics; they must stay bounded and
// must not carry host details beyond the message itself.
func sanitizeError(err error) string {
	if err == nil {
		return ""
	}
	msg := strings.TrimSpace(err.Error())
	if msg == "" {
		return "mcp: unknown error"
	}
	// Keep one line and bound the length so a misbehaving server cannot
	// flood diagnostics.
	if i := strings.IndexByte(msg, '\n'); i >= 0 {
		msg = strings.TrimSpace(msg[:i])
	}
	const maxErrorLen = 500
	if len(msg) > maxErrorLen {
		msg = strings.TrimSpace(msg[:maxErrorLen]) + "…"
	}
	if msg == "" {
		return "mcp: unknown error"
	}
	return msg
}

// sanitizeName bounds server names in synthesized errors. Config names are
// already constrained, but direct callers may pass arbitrary strings.
func sanitizeName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "<unknown>"
	}
	if len(name) > 64 {
		name = name[:64] + "…"
	}
	return name
}
