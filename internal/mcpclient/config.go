package mcpclient

import (
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"q-agent/internal/config"
)

// Timeout defaults applied when a server's connect_timeout/call_timeout is
// empty. They mirror internal/config's documented defaults; the canonical
// values live there so the loader and this package never disagree.
const (
	// DefaultConnectTimeout bounds MCP setup per task when
	// mcp.servers.<name>.connect_timeout is empty.
	DefaultConnectTimeout = config.DefaultMCPConnectTimeout
	// DefaultCallTimeout bounds each MCP tool call when
	// mcp.servers.<name>.call_timeout is empty.
	DefaultCallTimeout = config.DefaultMCPCallTimeout
)

// SDK type aliases pin the official Go SDK v1.8.0 surface that later issues
// build on. #46 (lifecycle) constructs sessions from these types; #47/#48
// supply stdio and Streamable HTTP transports. No lifecycle or transport
// logic lives in this configuration-only step.
type (
	// SDKClient is the official SDK client. Later issues create one per
	// agent task and connect it over the server's transport.
	SDKClient = mcp.Client
	// SDKClientSession is one logical MCP connection. Later issues scope
	// each session to a single agent task with deterministic cleanup.
	SDKClientSession = mcp.ClientSession
	// SDKTransport is the official SDK transport abstraction. Later issues
	// provide stdio (#47) and credential-free Streamable HTTP (#48)
	// implementations; this step defines no transports.
	SDKTransport = mcp.Transport
)

// Active reports whether MCP must start: true when at least one
// [mcp.servers.*] entry is enabled. With no enabled servers MCP performs no
// startup, spawns no child process, and opens no network connection.
func Active(cfg config.Config) bool {
	return cfg.MCPActive()
}

// EnabledServers returns the enabled subset of [mcp.servers.*] entries.
// Later issues dial exactly these servers per agent task.
func EnabledServers(cfg config.Config) map[string]config.MCPServer {
	return cfg.EnabledMCPServers()
}

// ConnectTimeout returns the effective setup timeout for one server,
// falling back to DefaultConnectTimeout when empty or invalid. Load-time
// validation already rejects invalid values; the fallback keeps direct
// struct literals usable in tests and later issues.
func ConnectTimeout(s config.MCPServer) time.Duration {
	return s.ConnectTimeoutDuration()
}

// CallTimeout returns the effective per-call timeout for one server,
// falling back to DefaultCallTimeout when empty or invalid.
func CallTimeout(s config.MCPServer) time.Duration {
	return s.CallTimeoutDuration()
}
