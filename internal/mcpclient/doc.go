// Package mcpclient integrates optional Model Context Protocol servers
// using the official Go SDK (github.com/modelcontextprotocol/go-sdk v1.8.0,
// targeting MCP 2026-07-28 with SDK-supported backward compatibility).
//
// Scope of issue #45 (this package today): configuration only. This package
// exposes the timeout defaults and the enabled-server helpers over
// internal/config's [mcp.servers.<name>] tables, plus SDK type aliases as
// extension points. It performs no lifecycle, transport, discovery,
// execution, or runtime work: no client is constructed, no child process is
// spawned, and no network connection is opened here.
//
// Extension points for later issues (do not implement here):
//   - #46 task lifecycle and capability handling builds sessions on
//     SDKClient/SDKClientSession and SDKTransport.
//   - #47 stdio transport in host/sandbox modes consumes MCPServer.Command
//     and MCPServer.Args.
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
