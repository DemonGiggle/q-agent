// Package mcpclient integrates optional Model Context Protocol servers
// using the official Go SDK (github.com/modelcontextprotocol/go-sdk v1.8.0,
// targeting MCP 2026-07-28 with SDK-supported backward compatibility).
//
// Scope of issues #45-#47 (this package today): configuration, lifecycle,
// and stdio transport. This package exposes the timeout defaults and the
// enabled-server helpers over internal/config's [mcp.servers.<name>] tables,
// task-scoped lifecycle over SDKClient/SDKClientSession and SDKTransport,
// and host/sandbox stdio transports consuming MCPServer.Command and
// MCPServer.Args. It performs no discovery,
// execution, or runtime work: no network connection is opened here.
//
// Extension points for later issues (do not implement here):
//   - #48 credential-free Streamable HTTP transport consumes MCPServer.URL.
//   - #49 tool discovery and schema adaptation, #50 execution and result
//     normalization, #51 runtime/diagnostics wiring, #52 docs and coverage.
//
// Policy inherited from the epic: named servers are inactive unless enabled;
// configuration takes effect after restart; transports are credential-free
// stdio commands or Streamable HTTP endpoints (HTTPS anywhere, HTTP on
// loopback only). Credential fields, custom headers, OAuth, and
// environment-secret injection are excluded.
package mcpclient
