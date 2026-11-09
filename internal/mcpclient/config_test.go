package mcpclient

import (
	"testing"
	"time"

	"q-agent/internal/config"
)

func TestInactiveByDefault(t *testing.T) {
	cfg := config.Defaults()
	if Active(cfg) {
		t.Fatal("MCP must be inactive with no servers configured")
	}
	if len(EnabledServers(cfg)) != 0 {
		t.Fatalf("no servers expected, got %v", EnabledServers(cfg))
	}
}

func TestActiveRequiresEnabledServer(t *testing.T) {
	cfg := config.Defaults()
	cfg.MCP.Servers = map[string]config.MCPServer{
		"disabled": {Enabled: false, Transport: "stdio", Command: "true"},
	}
	if Active(cfg) {
		t.Fatal("MCP must be inactive when all servers are disabled")
	}
	cfg.MCP.Servers["on"] = config.MCPServer{Enabled: true, Transport: "stdio", Command: "true"}
	if !Active(cfg) {
		t.Fatal("MCP must be active with one enabled server")
	}
	enabled := EnabledServers(cfg)
	if len(enabled) != 1 || enabled["on"].Command != "true" {
		t.Fatalf("enabled subset wrong: %v", enabled)
	}
}

func TestTimeoutHelpers(t *testing.T) {
	defConnect, _ := time.ParseDuration(DefaultConnectTimeout)
	defCall, _ := time.ParseDuration(DefaultCallTimeout)
	if defConnect <= 0 || defCall <= 0 {
		t.Fatalf("defaults must parse as positive durations: %q %q", DefaultConnectTimeout, DefaultCallTimeout)
	}
	empty := config.MCPServer{}
	if got := ConnectTimeout(empty); got != defConnect {
		t.Fatalf("empty connect timeout = %v, want %v", got, defConnect)
	}
	if got := CallTimeout(empty); got != defCall {
		t.Fatalf("empty call timeout = %v, want %v", got, defCall)
	}
	custom := config.MCPServer{ConnectTimeout: "5s", CallTimeout: "90s"}
	if got := ConnectTimeout(custom); got != 5*time.Second {
		t.Fatalf("custom connect timeout = %v", got)
	}
	if got := CallTimeout(custom); got != 90*time.Second {
		t.Fatalf("custom call timeout = %v", got)
	}
	if DefaultConnectTimeout != config.DefaultMCPConnectTimeout ||
		DefaultCallTimeout != config.DefaultMCPCallTimeout {
		t.Fatal("mcpclient defaults must mirror config defaults")
	}
}
