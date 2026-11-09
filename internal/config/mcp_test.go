package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeMCPConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestMCPDefaultsInactive(t *testing.T) {
	c := Defaults()
	if c.MCPActive() {
		t.Fatal("MCP must be inactive by default")
	}
	if len(c.EnabledMCPServers()) != 0 {
		t.Fatalf("no enabled servers expected: %v", c.EnabledMCPServers())
	}
	if DefaultMCPConnectTimeout != "10s" || DefaultMCPCallTimeout != "60s" {
		t.Fatalf("unexpected timeout defaults: %q %q", DefaultMCPConnectTimeout, DefaultMCPCallTimeout)
	}
	// The shipped example carries only disabled examples, so it loads with
	// MCP inactive and unchanged behavior.
	example, err := Load(filepath.Join("..", "..", "config.example.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if example.MCPActive() {
		t.Fatal("example config must leave MCP inactive")
	}
}

func TestMCPValidServers(t *testing.T) {
	path := writeMCPConfig(t, `[mcp.servers.files]
enabled = true
transport = "stdio"
command = "/usr/bin/mcp-files"
args = ["--stdio"]
[mcp.servers.docs]
enabled = false
transport = "http"
url = "https://mcp.example.com/mcp"
[mcp.servers.loop]
enabled = true
transport = "http"
url = "http://127.0.0.1:8787/mcp"
connect_timeout = "5s"
call_timeout = "90s"
`)
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !c.MCPActive() {
		t.Fatal("MCP must be active with enabled servers")
	}
	files := c.MCP.Servers["files"]
	if files.ConnectTimeout != "10s" || files.CallTimeout != "60s" {
		t.Fatalf("timeout defaults not applied: %+v", files)
	}
	if got := files.ConnectTimeoutDuration(); got.String() != "10s" {
		t.Fatalf("connect default duration = %v", got)
	}
	loop := c.MCP.Servers["loop"]
	if loop.ConnectTimeout != "5s" || loop.CallTimeout != "90s" {
		t.Fatalf("explicit timeouts not kept: %+v", loop)
	}
	enabled := c.EnabledMCPServers()
	if len(enabled) != 2 || enabled["files"].Command == "" {
		t.Fatalf("enabled subset wrong: %v", enabled)
	}
}

func TestMCPInvalidServers(t *testing.T) {
	for _, tc := range []struct{ name, table string }{
		{"missing transport", "[mcp.servers.a]\nenabled = false\ncommand = 'x'"},
		{"bad transport", "[mcp.servers.a]\ntransport = 'sse'\ncommand = 'x'"},
		{"stdio missing command", "[mcp.servers.a]\ntransport = 'stdio'"},
		{"stdio with url", "[mcp.servers.a]\ntransport = 'stdio'\ncommand = 'x'\nurl = 'https://example.com'"},
		{"http missing url", "[mcp.servers.a]\ntransport = 'http'"},
		{"http with command", "[mcp.servers.a]\ntransport = 'http'\nurl = 'https://example.com'\ncommand = 'x'"},
		{"http with args", "[mcp.servers.a]\ntransport = 'http'\nurl = 'https://example.com'\nargs = ['x']"},
		{"bad timeout", "[mcp.servers.a]\ntransport = 'stdio'\ncommand = 'x'\nconnect_timeout = '0s'"},
		{"unparseable timeout", "[mcp.servers.a]\ntransport = 'stdio'\ncommand = 'x'\ncall_timeout = 'forever'"},
		{"timeout over max", "[mcp.servers.a]\ntransport = 'stdio'\ncommand = 'x'\ncall_timeout = '11m'"},
		{"non-http url", "[mcp.servers.a]\ntransport = 'http'\nurl = 'ftp://example.com/x'"},
		{"http non-loopback", "[mcp.servers.a]\ntransport = 'http'\nurl = 'http://example.com/mcp'"},
		{"url with userinfo", "[mcp.servers.a]\ntransport = 'http'\nurl = 'https://user:pass@example.com/mcp'"},
		{"url without host", "[mcp.servers.a]\ntransport = 'http'\nurl = 'https:///mcp'"},
		{"bad name dots", "[mcp.servers.'a.b']\ntransport = 'stdio'\ncommand = 'x'"},
		{"bad name chars", "[mcp.servers.'a b']\ntransport = 'stdio'\ncommand = 'x'"},
		{"unknown field", "[mcp.servers.a]\ntransport = 'stdio'\ncommand = 'x'\nheaders = ['a']"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writeMCPConfig(t, tc.table+"\n")
			_, err := Load(path)
			if err == nil {
				t.Fatalf("invalid MCP server accepted: %s", tc.table)
			}
			if !strings.Contains(err.Error(), "mcp.servers") {
				t.Fatalf("error must identify the server: %v", err)
			}
		})
	}
}

func TestMCPKeyHandling(t *testing.T) {
	if !IsValidKey("mcp.servers.docs.enabled") || !IsValidKey("mcp.servers.docs.args") {
		t.Fatal("valid MCP keys rejected")
	}
	if IsValidKey("mcp.servers.docs.headers") || IsValidKey("mcp.servers") || IsValidKey("mcp") {
		t.Fatal("invalid MCP keys accepted")
	}
	if _, _, ok := ParseMCPServersKey("provider.model"); ok {
		t.Fatal("non-MCP key parsed as MCP")
	}
	if _, err := ParseValue("mcp.servers.docs.enabled", "true"); err != nil {
		t.Fatal(err)
	}
	if _, err := ParseValue("mcp.servers.docs.args", `["a", "b"]`); err != nil {
		t.Fatal(err)
	}
	if v, err := ParseValue("mcp.servers.docs.url", "https://example.com"); err != nil || v != "https://example.com" {
		t.Fatalf("string field round-trip: %v %v", v, err)
	}
	if _, err := ParseValue("mcp.servers.docs.enabled", "yes"); err == nil {
		t.Fatal("non-TOML bool accepted")
	}
	if _, err := ParseValue("mcp.servers.'bad name'.enabled", "true"); err == nil {
		t.Fatal("bad server name accepted by ParseValue")
	}
	if hint := UnknownKeyHint("mcp.servers.docs.headers"); hint == "" {
		t.Fatal("missing hint for unknown MCP field")
	}
	if hint := MCPKeyHint("mcp.servers.docs.enabled"); hint != "" {
		t.Fatalf("valid key should have no hint: %q", hint)
	}
	path := writeMCPConfig(t, "")
	if err := Update(path, map[string]any{
		"mcp.servers.docs.enabled":   true,
		"mcp.servers.docs.transport": "http",
		"mcp.servers.docs.url":       "https://mcp.example.com/mcp",
	}); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.MCP.Servers["docs"].Enabled || cfg.MCP.Servers["docs"].URL == "" {
		t.Fatalf("MCP round-trip failed: %+v", cfg.MCP.Servers)
	}
	if err := Update(path, map[string]any{"mcp.servers.docs.command": "x"}); err == nil {
		t.Fatal("mutually exclusive stdio/http mix accepted by Update")
	}
}
