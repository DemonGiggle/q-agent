# q-agent MCP

[← Overview](../README.md) · [Configuration & operations](operations.md) · [Console guide](console.md) · [Example configuration](../config.example.toml)

q-agent is an MCP **client**. It can call tools from external
[Model Context Protocol](https://modelcontextprotocol.io) servers and offer
them to the model alongside its built-in tools (`read`, `shell`, `web_fetch`,
…). It never *serves* its own tools over MCP.

MCP is **optional and off by default**: with no
`[mcp.servers.*]` table with `enabled = true`, q-agent spawns no child
process and opens no network connection for MCP.

## 1. How it fits together

```text
Telegram chat / scheduled task
        |
        v
agent task starts ── dial every enabled server fresh for this task
        |              stdio: spawn command · http: open Streamable HTTP session
        v
discover tools/list ── publish one snapshot: server__tool names
        |
        v
model calls server__tool ── routed to that server, result returned as text
        |
        v
task ends ── every MCP session closed (completion, cancel, timeout, shutdown)
```

- **Shared everywhere.** Every authorized chat — and every scheduled agent
  task — gets the same enabled servers. There is no per-chat scoping and no
  per-call approval. Enabling a server trusts all authorized chats with its
  tools.
- **Isolated per task.** Servers are dialed fresh for each task and released
  afterwards. One server failing never disables healthy servers or built-in
  tools; it just shows as *unavailable* with a one-line reason.
- **Tools appear as `server__tool`.** For example server `files` exposing
  `read_file` becomes `files__read_file`. Collisions (across servers, or
  with built-in / schedule tools) get a deterministic `_2`, `_3`, … suffix.
  Invalid tools are *quarantined* with a sanitized reason instead of
  poisoning the whole request.
- **Results are untrusted third-party data.** They are redacted, bounded,
  and wrapped like any other tool result before reaching the model.

## 2. Quick start (5 minutes)

Pick **one** transport to start with.

### Option A — stdio (local command)

Runs a local MCP server binary as a child process. Good for filesystem,
shell, or custom tools on the same machine.

```toml
[mcp.servers.files]
enabled = true
transport = "stdio"
command = "npx"
args = ["-y", "@modelcontextprotocol/server-filesystem", "/tmp/mcp-demo"]
```

Prerequisites: `npx` (or your server binary) installed where the task runs
(see [host vs sandbox](#5-host-vs-sandbox-stdio-caveat) — this is the step
people get wrong), plus the directory it should serve (`mkdir -p
/tmp/mcp-demo`).

### Option B — Streamable HTTP (remote endpoint)

Dials a URL over Streamable HTTP. Good for hosted services.

```toml
[mcp.servers.docs]
enabled = true
transport = "http"
url = "https://mcp.example.com/mcp"
```

Rules: `https://` anywhere; plain `http://` only for loopback
(`127.0.0.0/8`, `::1`, `localhost`). No `user:pass@` credentials in the URL,
no custom headers, no OAuth — credential endpoints are rejected.

### Enable it

1. Add the table above to `~/.config/q-agent/config.toml`
   (default path; override with `--config FILE`).
2. Validate without touching the network:

   ```sh
   ./bin/q-agent config check
   ```

3. **Restart** the service — MCP edits apply at startup, there is no live
   reload:

   ```sh
   # foreground
   ./bin/q-agent serve
   # systemd
   systemctl --user restart q-agent
   ```

4. Verify. The service log prints one line per task, e.g.:

   ```text
   mcp: 1/2 servers available, 5 tools, 0 quarantined
   ```

   and the chat's system prompt lists available servers with tool counts
   plus unavailable servers with a one-line reason. Ask the bot something
   that uses the new tool, e.g. for the filesystem example above:
   *“List /tmp/mcp-demo with files__list_directory”*.

Same steps via the console (no hand-editing TOML):

```sh
q-agent config set mcp.servers.files.enabled true
q-agent config set mcp.servers.files.transport stdio
q-agent config set mcp.servers.files.command npx
q-agent config set mcp.servers.files.args '["-y", "@modelcontextprotocol/server-filesystem", "/tmp/mcp-demo"]'
q-agent config check
# then restart as above
```

For HTTP:

```sh
q-agent config set mcp.servers.docs.enabled true
q-agent config set mcp.servers.docs.transport http
q-agent config set mcp.servers.docs.url https://mcp.example.com/mcp
q-agent config check
```

## 3. Configuration reference

Each server is one named `[mcp.servers.<name>]` table.

| Field | Required for | Meaning / default |
| --- | --- | --- |
| `enabled` | both | `true` to use this server. Default `false`. |
| `transport` | both | `"stdio"` or `"http"`. Required. |
| `command` | `stdio` | Binary to spawn (PATH or absolute path). `url` must be empty. |
| `args` | `stdio` | Argument list, e.g. `["--stdio"]`. Must be empty for `http`. |
| `url` | `http` | Streamable HTTP endpoint, e.g. `https://mcp.example.com/mcp`. `command`/`args` must be empty. |
| `connect_timeout` | both | Setup bound per task. Default `"10s"`. |
| `call_timeout` | both | Bound per tool call. Default `"60s"`. |

Additional rules:

- **Name:** 1–64 chars, letters/digits/`_`/`-`, starting with a letter or
  digit. Dots are rejected so table names never nest.
- **Timeouts:** positive durations up to `10m` (`"500ms"`, `"10s"`, `"2m"`).
  Empty means the default.
- **Validation** names the server and field, e.g.
  `mcp.servers."files".command is required for stdio transport`.
  `q-agent config check` reports the same rules without network access.

Minimal complete file excerpt:

```toml
# one stdio + one http server, everything else defaults
[mcp.servers.files]
enabled = true
transport = "stdio"
command = "npx"
args = ["-y", "@modelcontextprotocol/server-filesystem", "/tmp/mcp-demo"]
connect_timeout = "10s"
call_timeout = "60s"

[mcp.servers.docs]
enabled = false
transport = "http"
url = "https://mcp.example.com/mcp"
```

Full commented reference lives in
[`config.example.toml`](../config.example.toml) under `[mcp]`.

## 4. Simple end-to-end example

This is the smallest realistic setup: expose one host directory through the
public filesystem server and call it from Telegram.

```sh
# 1. prerequisites
mkdir -p /tmp/mcp-demo
echo hello > /tmp/mcp-demo/greeting.txt
which npx   # must succeed in the task's environment (see below)

# 2. configure (host mode default: [sandbox] enabled = false)
q-agent config set mcp.servers.files.enabled true
q-agent config set mcp.servers.files.transport stdio
q-agent config set mcp.servers.files.command npx
q-agent config set mcp.servers.files.args '["-y", "@modelcontextprotocol/server-filesystem", "/tmp/mcp-demo"]'
q-agent config check

# 3. restart so the new table takes effect
systemctl --user restart q-agent   # or: ./bin/q-agent serve

# 4. in Telegram, ask the bot:
#    "What files are in /tmp/mcp-demo? Use files__list_directory."
#    then: "Read /tmp/mcp-demo/greeting.txt with files__read_text_file."
```

What you should see:

- Log: `mcp: 1/1 servers available, N tools, 0 quarantined`.
- The model calls `files__list_directory` / `files__read_text_file` (exact
  tool names depend on the server version; the `files__` prefix is always
  the server name).
- If the server is down, the model is told it is unavailable with a
  one-line reason — commands, arguments, URLs, and environment never appear
  in logs, prompts, or errors.

Prefer HTTP? Replace step 2 with:

```sh
q-agent config set mcp.servers.docs.enabled true
q-agent config set mcp.servers.docs.transport http
q-agent config set mcp.servers.docs.url https://mcp.example.com/mcp
```

then restart and ask the bot to use a `docs__<tool>` name.

## 5. Host vs sandbox: stdio caveat

This is the step people get wrong, so here it is in one place.

- **Host mode** (`[sandbox] enabled = false`, the default): `command`
  resolves on the **host** — `PATH` or an absolute path, run as the service
  account, in the chat's working directory.
- **Sandbox mode** (`enabled = true`): `command` must exist **inside the
  chat's Debian image** (e.g. `/workspace`), because the sandbox cannot see
  host paths. The child starts with `Dir /workspace` and a filtered
  environment (credential-shaped variables stripped).

A sandbox setup or boundary failure **never falls back to host execution**:
the server just reports unavailable. If your server shows unavailable,
check it is reachable from the task's mode and that you restarted after
saving the config. HTTP servers work in either mode (they need no local
binary).

## 6. Security model (read before enabling)

- Enabling a server trusts **all authorized chats and all scheduled tasks**
  with its tools. There is no per-chat allowlist and no approval prompt.
- MCP results are **untrusted**: treated like web content, redacted and
  truncated before the model sees them.
- Transports are **credential-free by design**: no userinfo in URLs, no
  `Authorization`/`Cookie` headers, no OAuth flow, no secret injection into
  environments. The child inherits a filtered environment only. API keys
  stay in host memory and never enter the sandbox.
- Diagnostics are **sanitized end to end**: logs, status lines, and errors
  carry only server names, transport kinds, versions, tool counts, and
  truncated reasons. Commands, arguments, URLs, and host paths never leak
  there — check your config file directly when debugging.

## 7. Troubleshooting

| Symptom | Check |
| --- | --- |
| Server shows unavailable | `command` on `PATH` (host) or inside image (sandbox)? Endpoint reachable from task mode? `q-agent config check` clean? **Restarted** after edit? |
| `mcp: 0/1 servers available` | Log one-liner + chat prompt reason name the server (not the secret). Fix that server; healthy ones keep working. |
| Validation error naming server + field | `command` set for `stdio` and empty for `http` (and vice versa for `url`/`args`); `transport` is `stdio\|http`; timeouts positive ≤ `10m`; HTTP loopback rule. |
| Tools missing / quarantined count > 0 | Server's `tools/list` schema invalid or over bounds; check server logs. One bad tool never blocks the rest. |
| Config edit didn't apply | MCP has no live reload — restart. `config set` prints `{"saved":true,"restart_required":true}` as a reminder. |

Useful commands:

```sh
q-agent config get mcp.servers.files.transport
q-agent config show --json | grep -i mcp
q-agent config check
journalctl --user -u q-agent -f   # watch the per-task mcp: line
```

## 8. What is not supported

Standalone resources/prompts, roots, sampling, elicitation, OAuth or stored
credentials, custom headers, environment-secret injection, legacy HTTP+SSE
transport, and bundled server binaries. q-agent dials `tools/list` +
`tools/call` over stdio or Streamable HTTP only.
