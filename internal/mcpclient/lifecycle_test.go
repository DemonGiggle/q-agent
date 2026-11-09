package mcpclient

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"q-agent/internal/config"
)

// deadTransport simulates a transport whose connection is already broken:
// dial succeeds, but the handshake outcome is unknown (ambiguous).
type deadTransport struct{}

func (deadTransport) Connect(context.Context) (mcp.Connection, error) {
	return deadConn{}, nil
}

type deadConn struct{}

func (deadConn) Read(context.Context) (jsonrpc.Message, error) {
	return nil, errors.New("connection closed")
}

func (deadConn) Write(context.Context, jsonrpc.Message) error {
	return errors.New("connection closed")
}

func (deadConn) Close() error { return nil }

func (deadConn) SessionID() string { return "" }

var testServerImpl = &mcp.Implementation{Name: "test-server", Version: "v0.0.1"}

type hiArgs struct {
	Name string `json:"name"`
}

func sayHiForTest(_ context.Context, _ *mcp.CallToolRequest, args hiArgs) (*mcp.CallToolResult, any, error) {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "hi " + args.Name}}}, nil, nil
}

// fixture connects an SDK server over in-memory transport and returns the
// client-side transport plus the server session for capability inspection.
func fixture(t *testing.T, ctx context.Context, server *mcp.Server) (mcp.Transport, *mcp.ServerSession) {
	t.Helper()
	ct, st := mcp.NewInMemoryTransports()
	ss, err := server.Connect(ctx, st, nil)
	if err != nil {
		t.Fatalf("server Connect: %v", err)
	}
	t.Cleanup(func() { _ = ss.Close() })
	return ct, ss
}

func enabledServer(transport string) config.MCPServer {
	return config.MCPServer{
		Enabled:        true,
		Transport:      transport,
		ConnectTimeout: "5s",
		CallTimeout:    "5s",
	}
}

func TestModernConnect(t *testing.T) {
	ctx := context.Background()
	srv := mcp.NewServer(testServerImpl, nil)
	mcp.AddTool(srv, &mcp.Tool{Name: "greet", Description: "say hi"}, sayHiForTest)
	ct, _ := fixture(t, ctx, srv)

	servers := map[string]config.MCPServer{"modern": enabledServer("stdio")}
	var calls atomic.Int32
	task := StartTask(ctx, servers, func(ctx context.Context, name string, _ config.MCPServer) (SDKTransport, error) {
		calls.Add(1)
		if name != "modern" {
			t.Fatalf("unexpected server %q", name)
		}
		return ct, nil
	})
	t.Cleanup(func() { _ = task.Close() })

	if got := calls.Load(); got != 1 {
		t.Fatalf("dial called %d times, want 1 (no auto-replay)", got)
	}
	status, ok := task.Status("modern")
	if !ok {
		t.Fatal("missing status for modern server")
	}
	if !status.Available {
		t.Fatalf("modern server unavailable: %q", status.Error)
	}
	if status.ProtocolVersion != "2026-07-28" {
		t.Fatalf("modern protocol version = %q, want 2026-07-28", status.ProtocolVersion)
	}
	if !status.HasTools {
		t.Fatal("modern server should report HasTools")
	}
	session, ok := task.Session("modern")
	if !ok || session == nil {
		t.Fatal("modern session missing")
	}
	ir := session.InitializeResult()
	if ir == nil || ir.ProtocolVersion != "2026-07-28" {
		t.Fatalf("InitializeResult version = %+v, want 2026-07-28", ir)
	}
}

func TestLegacyConnect(t *testing.T) {
	ctx := context.Background()
	srv := mcp.NewServer(testServerImpl, &mcp.ServerOptions{SupportedProtocolVersions: []string{"2025-11-25"}})
	mcp.AddTool(srv, &mcp.Tool{Name: "greet", Description: "say hi"}, sayHiForTest)
	ct, _ := fixture(t, ctx, srv)

	servers := map[string]config.MCPServer{"legacy": enabledServer("http")}
	task := StartTask(ctx, servers, func(context.Context, string, config.MCPServer) (SDKTransport, error) {
		return ct, nil
	})
	t.Cleanup(func() { _ = task.Close() })

	status, ok := task.Status("legacy")
	if !ok {
		t.Fatal("missing status for legacy server")
	}
	if !status.Available {
		t.Fatalf("legacy server unavailable: %q", status.Error)
	}
	if status.ProtocolVersion != "2025-11-25" {
		t.Fatalf("legacy protocol version = %q, want 2025-11-25", status.ProtocolVersion)
	}
	session, ok := task.Session("legacy")
	if !ok || session == nil {
		t.Fatal("legacy session missing")
	}
	if got := session.InitializeResult().ProtocolVersion; got != "2025-11-25" {
		t.Fatalf("legacy InitializeResult version = %q", got)
	}
}

func TestAdvertisesOnlyImplementedCapabilities(t *testing.T) {
	ctx := context.Background()
	// Legacy path carries client caps in initialize params; modern discover
	// populates the same server-side state from per-request _meta.
	for _, tc := range []struct {
		name    string
		options *mcp.ServerOptions
	}{
		{"modern", nil},
		{"legacy", &mcp.ServerOptions{SupportedProtocolVersions: []string{"2025-11-25"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := mcp.NewServer(testServerImpl, tc.options)
			ct, ss := fixture(t, ctx, srv)
			servers := map[string]config.MCPServer{"s": enabledServer("stdio")}
			task := StartTask(ctx, servers, func(context.Context, string, config.MCPServer) (SDKTransport, error) {
				return ct, nil
			})
			t.Cleanup(func() { _ = task.Close() })
			if st, _ := task.Status("s"); !st.Available {
				t.Fatalf("server unavailable: %q", st.Error)
			}
			params := ss.InitializeParams()
			if params == nil || params.Capabilities == nil {
				t.Fatal("server did not record client capabilities")
			}
			caps := params.Capabilities
			if caps.RootsV2 != nil || caps.Roots.ListChanged {
				t.Fatalf("roots must be disabled, got %+v", caps.RootsV2)
			}
			if caps.Sampling != nil {
				t.Fatalf("sampling must be disabled, got %+v", caps.Sampling)
			}
			if caps.Elicitation != nil {
				t.Fatalf("elicitation must be disabled, got %+v", caps.Elicitation)
			}
			if len(caps.Extensions) != 0 || len(caps.Experimental) != 0 {
				t.Fatalf("extensions/experimental must be disabled, got %+v %+v", caps.Extensions, caps.Experimental)
			}
		})
	}
}

func TestIncompatibleVersionFailsClearly(t *testing.T) {
	ctx := context.Background()
	// Legacy-only server forced to answer initialize with an unknown
	// version: discover falls back to initialize, then the client must
	// fail with a clear unsupported-version error (not ambiguous).
	srv2 := mcp.NewServer(testServerImpl, &mcp.ServerOptions{SupportedProtocolVersions: []string{"2025-11-25"}})
	srv2.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if method == "initialize" {
				return &mcp.InitializeResult{
					ProtocolVersion: "1999-01-01",
					Capabilities:    &mcp.ServerCapabilities{},
					ServerInfo:      testServerImpl,
				}, nil
			}
			return next(ctx, method, req)
		}
	})
	ct, _ := fixture(t, ctx, srv2)

	servers := map[string]config.MCPServer{"bad": enabledServer("stdio")}
	task := StartTask(ctx, servers, func(context.Context, string, config.MCPServer) (SDKTransport, error) {
		return ct, nil
	})
	t.Cleanup(func() { _ = task.Close() })

	status, _ := task.Status("bad")
	if status.Available {
		t.Fatal("incompatible server must not be available")
	}
	if _, ok := task.Session("bad"); ok {
		t.Fatal("incompatible server must have no session")
	}
	if !strings.Contains(strings.ToLower(status.Error), "unsupported protocol version") {
		t.Fatalf("incompatible error must mention unsupported protocol version, got %q", status.Error)
	}
	if IsAmbiguous(errors.New(status.Error)) {
		t.Fatal("sanitized status string must not itself be ambiguous")
	}
}

func TestUnavailableDoesNotDisableHealthy(t *testing.T) {
	ctx := context.Background()
	srv := mcp.NewServer(testServerImpl, nil)
	mcp.AddTool(srv, &mcp.Tool{Name: "greet", Description: "say hi"}, sayHiForTest)
	ct, _ := fixture(t, ctx, srv)

	servers := map[string]config.MCPServer{
		"good": enabledServer("stdio"),
		"down": enabledServer("http"),
	}
	task := StartTask(ctx, servers, func(_ context.Context, name string, _ config.MCPServer) (SDKTransport, error) {
		if name == "good" {
			return ct, nil
		}
		return nil, errors.New("dial tcp 127.0.0.1:1: connection refused")
	})
	t.Cleanup(func() { _ = task.Close() })

	if !task.Available("good") {
		st, _ := task.Status("good")
		t.Fatalf("healthy server must stay available, got %q", st.Error)
	}
	if task.Available("down") {
		t.Fatal("unavailable server must not be available")
	}
	down, _ := task.Status("down")
	if down.Available || down.Error == "" {
		t.Fatalf("unavailable server must record an error, got %+v", down)
	}
	if strings.Contains(down.Error, "secret") {
		t.Fatalf("error must be sanitized, got %q", down.Error)
	}
	// Ordinary tools stay usable: the Task only affects its own sessions.
	if _, ok := task.Session("good"); !ok {
		t.Fatal("healthy session must be usable alongside the failure")
	}
}

func TestAmbiguousFailureNoReplay(t *testing.T) {
	// Dial succeeds but the connection is already dead: the SDK handshake
	// cannot determine whether the server saw the request. Lifecycle must
	// surface this as ambiguous and must not retry automatically.
	servers := map[string]config.MCPServer{"flaky": enabledServer("stdio")}
	var calls atomic.Int32
	task := StartTask(context.Background(), servers, func(context.Context, string, config.MCPServer) (SDKTransport, error) {
		calls.Add(1)
		return deadTransport{}, nil
	})
	t.Cleanup(func() { _ = task.Close() })

	if got := calls.Load(); got != 1 {
		t.Fatalf("dial called %d times, want exactly 1 (never replay after ambiguous failure)", got)
	}
	status, _ := task.Status("flaky")
	if status.Available {
		t.Fatal("broken connection must not be available")
	}
	if !strings.Contains(strings.ToLower(status.Error), "ambiguous") || !strings.Contains(strings.ToLower(status.Error), "do not replay") {
		t.Fatalf("ambiguous failure must be surfaced without replay, got %q", status.Error)
	}
	if _, ok := task.Session("flaky"); ok {
		t.Fatal("ambiguous server must have no session")
	}

	// The marker API used by execution (#50) to avoid replays.
	amb := NewAmbiguousError("flaky", "connect", errors.New("connection closed"))
	if !IsAmbiguous(amb) || !errors.Is(amb, ErrAmbiguous) {
		t.Fatal("AmbiguousError must match ErrAmbiguous")
	}
	if IsAmbiguous(errors.New("dial refused")) {
		t.Fatal("definitive dial errors must not be ambiguous")
	}
}

func TestCloseIdempotentAndCleansPartialSetup(t *testing.T) {
	ctx := context.Background()
	srv := mcp.NewServer(testServerImpl, nil)
	mcp.AddTool(srv, &mcp.Tool{Name: "greet", Description: "say hi"}, sayHiForTest)
	ct, _ := fixture(t, ctx, srv)

	servers := map[string]config.MCPServer{
		"good": enabledServer("stdio"),
		"bad":  enabledServer("http"),
	}
	task := StartTask(ctx, servers, func(_ context.Context, name string, _ config.MCPServer) (SDKTransport, error) {
		if name == "good" {
			return ct, nil
		}
		return nil, errors.New("unavailable")
	})

	if !task.Available("good") {
		t.Fatal("good server should be available before close")
	}
	if err := task.Close(); err != nil {
		// Close joins session errors; a clean in-memory close is nil.
		t.Logf("close returned %v", err)
	}
	if task.Available("good") {
		t.Fatal("sessions must be gone after Close")
	}
	if _, ok := task.Session("good"); ok {
		t.Fatal("Session must be absent after Close")
	}
	// Repeated cleanup is deterministic and safe.
	if err := task.Close(); err != nil {
		t.Fatalf("second Close must return nil, got %v", err)
	}
	task.CloseTask()
	// Statuses remain for diagnostics after close.
	if _, ok := task.Status("good"); !ok {
		t.Fatal("statuses must survive Close for diagnostics")
	}
}

func TestSetupCancelAndTimeoutBounded(t *testing.T) {
	// Cancelled parent: no sessions, sanitized errors, safe cleanup.
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	task := StartTask(cancelled, map[string]config.MCPServer{"s": enabledServer("stdio")},
		func(ctx context.Context, _ string, _ config.MCPServer) (SDKTransport, error) {
			ct, _ := mcp.NewInMemoryTransports()
			return ct, nil
		})
	if task.Available("s") {
		t.Fatal("cancelled setup must not be available")
	}
	task.Close()
	task.Close()

	// Per-server timeout bounds a hanging dial.
	hanging := map[string]config.MCPServer{
		"slow": {Enabled: true, Transport: "stdio", ConnectTimeout: "50ms", CallTimeout: "5s"},
	}
	start := time.Now()
	task2 := StartTask(context.Background(), hanging, func(ctx context.Context, _ string, _ config.MCPServer) (SDKTransport, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	t.Cleanup(func() { _ = task2.Close() })
	elapsed := time.Since(start)
	if elapsed > 5*time.Second {
		t.Fatalf("setup was not bounded: took %v", elapsed)
	}
	if task2.Available("slow") {
		t.Fatal("timed-out server must not be available")
	}
}

func TestTasksDoNotShareState(t *testing.T) {
	ctx := context.Background()
	mkServer := func() mcp.Transport {
		srv := mcp.NewServer(testServerImpl, nil)
		ct, _ := fixture(t, ctx, srv)
		return ct
	}
	ct1, ct2 := mkServer(), mkServer()
	servers := map[string]config.MCPServer{"s": enabledServer("stdio")}

	first := StartTask(ctx, servers, func(context.Context, string, config.MCPServer) (SDKTransport, error) {
		return ct1, nil
	})
	second := StartTask(ctx, servers, func(context.Context, string, config.MCPServer) (SDKTransport, error) {
		return ct2, nil
	})
	s1, ok1 := first.Session("s")
	s2, ok2 := second.Session("s")
	if !ok1 || !ok2 {
		t.Fatal("both tasks should connect independently")
	}
	if s1 == s2 {
		t.Fatal("tasks must not share sessions")
	}
	// Closing one task ends only its own lifetime.
	_ = first.Close()
	if first.Available("s") {
		t.Fatal("closed task must not be available")
	}
	if !second.Available("s") {
		t.Fatal("second task must survive first task's close")
	}
	second.Close()
}

func TestSupportedVersionsIncludesModern(t *testing.T) {
	versions := SupportedProtocolVersions()
	found := false
	for _, v := range versions {
		if v == "2026-07-28" {
			found = true
		}
	}
	if !found {
		t.Fatalf("supported versions must include 2026-07-28, got %v", versions)
	}
	if len(versions) == 0 {
		t.Fatal("supported versions must not be empty")
	}
}
