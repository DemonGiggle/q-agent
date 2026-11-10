# q-agent MCP

[← Overview](../README.md) · [Configuration & operations](operations.md) · [Console guide](console.md) · [Example configuration](../config.example.toml)

q-agent is an MCP **client**. It calls tools from external
[Model Context Protocol](https://modelcontextprotocol.io) servers. It shows
those tools to the model next to its built-in tools (`read`, `shell`,
`web_fetch`, …). It never *serves* its own tools over MCP.

MCP is **optional and off by default**. If no server has `enabled = true`,
q-agent starts no child process and opens no network connection for MCP.

## 1. How it fits together

```text
Telegram chat / scheduled task
        |
        v
agent task starts: dial every enabled server again from scratch
  stdio: start a local program · http: open a Streamable HTTP session
        |
        v
discover tools: publish one snapshot with names like server__tool
        |
        v
model calls server__tool: q-agent routes the call to that server
        |
        v
task ends: close every MCP connection
```

What this means for you:

- **Same servers for everyone.** Every authorized chat and every scheduled
  task uses the same enabled servers. There is no per-chat limit and no
  approval prompt. Only enable servers you trust with all chats.
- **Fresh connection per task.** A new task dials again. The last task
  always closes its connections, even on cancel, timeout, or shutdown.
- **One bad server does not stop the rest.** A failing server shows as
  *unavailable* with a short reason. Healthy servers and built-in tools
  keep working.
- **Tools are named `server__tool`.** If server `demo` has a tool `greet`,
  the model sees `demo__greet`. Name clashes get a suffix such as `_2`.
  Broken tools are *quarantined* so one bad tool cannot break the request.
- **Results are untrusted.** MCP output is treated like web content. It is
  redacted and size-limited before the model sees it.

## 2. Quick start (5 minutes)

Pick **one** transport.

### Option A — stdio (a program on your machine)

With stdio, q-agent starts a program and talks to it over stdin and stdout.
That program must be an MCP server. A normal shell command such as `ls`
does not speak MCP, so it will not work here.

The simplest working setup is a small Python server with one tool.

Create `~/mcp-demo/server.py`:

```python
from mcp.server.fastmcp import FastMCP

mcp = FastMCP("demo")

@mcp.tool()
def greet(name: str) -> str:
    """Say hello to someone."""
    return f"Hello, {name}!"

if __name__ == "__main__":
    mcp.run()
```

Install the SDK once:

```sh
pip install mcp
```

Point q-agent at the file. The command stays simple: run `python3` with
one file argument.

```toml
[mcp.servers.demo]
enabled = true
transport = "stdio"
command = "python3"
args = ["/home/you/mcp-demo/server.py"]
```

Replace `/home/you` with your real home path. Use an absolute path so
q-agent finds the file from any working directory.

Need a real-world server instead? Use the public filesystem server:

```toml
[mcp.servers.files]
enabled = true
transport = "stdio"
command = "npx"
args = ["-y", "@modelcontextprotocol/server-filesystem", "/tmp/mcp-demo"]
```

This needs `npx` and the `/tmp/mcp-demo` directory. The Python example
above is easier for a first test.

### Option B — HTTP (a URL to call)

With HTTP, q-agent calls a remote endpoint over Streamable HTTP. Use this
for hosted services. No local program is needed.

```toml
[mcp.servers.docs]
enabled = true
transport = "http"
url = "https://mcp.example.com/mcp"
```

Your URL must follow these rules:

- Use `https://` for remote servers.
- Use `http://` only on the same machine. Allowed names are `localhost`,
  `127.0.0.0/8`, and `::1`.
- Do not put a login in the URL. `https://user:pass@example.com/mcp` is
  rejected.
- q-agent sends no login headers and runs no OAuth flow. Servers that need
  a login will not work.

### Enable it

1. Add one table from above to `~/.config/q-agent/config.toml`.
2. Check the file without touching the network:

   ```sh
   ./bin/q-agent config check
   ```

3. **Restart.** MCP settings apply only at startup. There is no live
   reload.

   ```sh
   # foreground
   ./bin/q-agent serve
   # systemd
   systemctl --user restart q-agent
   ```

4. Verify. The service log prints one line per task, for example:

   ```text
   mcp: 1/2 servers available, 5 tools, 0 quarantined
   ```

   The model also sees which servers are up and which are down, with a
   short reason for each down server. Then ask the bot to use the new
   tool. For the Python example: *“Call demo__greet with name Ana”*.

You can do the same steps without editing TOML by hand:

```sh
q-agent config set mcp.servers.demo.enabled true
q-agent config set mcp.servers.demo.transport stdio
q-agent config set mcp.servers.demo.command python3
q-agent config set mcp.servers.demo.args '["/home/you/mcp-demo/server.py"]'
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

Each server is one `[mcp.servers.<name>]` table.

| Field | Needed for | Meaning |
| --- | --- | --- |
| `enabled` | both | Set `true` to use this server. Default is `false`. |
| `transport` | both | `"stdio"` or `"http"`. Required. |
| `command` | `stdio` | Program to start. Use a PATH name or an absolute path. Leave `url` empty. |
| `args` | `stdio` | Arguments for the program, for example `["server.py"]`. Leave empty for `http`. |
| `url` | `http` | Streamable HTTP endpoint. Leave `command` and `args` empty. |
| `connect_timeout` | both | Time allowed to connect, per task. Default `"10s"`. |
| `call_timeout` | both | Time allowed per tool call. Default `"60s"`. |

Three more rules:

- **Server name:** 1–64 characters. Letters, digits, `_`, `-`. It must
  start with a letter or digit. Dots are not allowed.
- **Timeouts:** a positive duration up to `10m`. Examples: `"500ms"`,
  `"10s"`, `"2m"`. Empty means the default.
- **Errors name the server and field.** Example:
  `mcp.servers."demo".command is required for stdio transport`.
  `q-agent config check` reports the same errors without using the network.

Full example with defaults shown:

```toml
[mcp.servers.demo]
enabled = true
transport = "stdio"
command = "python3"
args = ["/home/you/mcp-demo/server.py"]
connect_timeout = "10s"
call_timeout = "60s"

[mcp.servers.docs]
enabled = false
transport = "http"
url = "https://mcp.example.com/mcp"
```

The complete commented reference is in
[`config.example.toml`](../config.example.toml) under `[mcp]`.

## 4. Simple end-to-end example

This uses the Python `greet` server from section 2.

```sh
# 1. Create the server file
mkdir -p ~/mcp-demo
# (save the server.py code from section 2 into ~/mcp-demo/server.py)
pip install mcp

# 2. Configure it (host mode is the default: [sandbox] enabled = false)
q-agent config set mcp.servers.demo.enabled true
q-agent config set mcp.servers.demo.transport stdio
q-agent config set mcp.servers.demo.command python3
q-agent config set mcp.servers.demo.args '["/home/you/mcp-demo/server.py"]'
q-agent config check

# 3. Restart so the new table takes effect
systemctl --user restart q-agent   # or: ./bin/q-agent serve

# 4. In Telegram, ask the bot:
#    "Call demo__greet with name Ana."
```

What you should see:

- Log line: `mcp: 1/1 servers available, 1 tool, 0 quarantined`.
- The model calls `demo__greet` and replies with `Hello, Ana!`.
- If the server is down, the model is told it is unavailable with a short
  reason. Commands, arguments, URLs, and environment values never appear in
  logs or errors. Check your config file directly when debugging.

To try HTTP instead, replace step 2 with:

```sh
q-agent config set mcp.servers.docs.enabled true
q-agent config set mcp.servers.docs.transport http
q-agent config set mcp.servers.docs.url https://mcp.example.com/mcp
```

Then restart and ask the bot to use a `docs__<tool>` name.

## 5. Host vs sandbox: where the program runs

This is the step people get wrong.

- **Host mode** (`[sandbox] enabled = false`, the default): `command` runs
  on the **host**. It must be on `PATH` or given as an absolute path. It
  runs as the service account in the chat's working directory.
- **Sandbox mode** (`enabled = true`): `command` must exist **inside the
  chat's Debian image**, for example under `/workspace`. The sandbox cannot
  see host paths such as `/home/you`. The program starts in `/workspace`
  with a cleaned environment.

A sandbox failure never falls back to the host. The server just shows as
unavailable. If your server shows unavailable, check two things: is the
program installed where the task runs, and did you restart after saving the
config? HTTP servers work in both modes because they need no local program.

## 6. Security model (read before enabling)

- Enabling a server trusts **all authorized chats and all scheduled tasks**
  with its tools. There is no per-chat limit and no approval prompt.
- MCP results are **untrusted**. They are redacted and cut to a safe size
  before the model sees them.
- Transports are **credential-free by design**. URLs cannot contain a
  login. q-agent sends no `Authorization` or `Cookie` headers, runs no
  OAuth, and injects no secrets into the program's environment. API keys
  stay in host memory and never enter the sandbox.
- Logs and errors are **sanitized**. They carry only the server name,
  transport type, version, tool count, and a short reason. They never
  include commands, arguments, URLs, or host paths.

## 7. Troubleshooting

| Symptom | What to check |
| --- | --- |
| Server shows unavailable | Is the program on `PATH` (host) or inside the image (sandbox)? Is the endpoint reachable from the task mode? Did you restart after saving? |
| `mcp: 0/1 servers available` | Read the one-line reason in the log or chat. Fix that server. Healthy servers keep working. |
| Validation error names a server and field | For `stdio`, set `command` and leave `url` empty. For `http`, set `url` and leave `command` and `args` empty. `transport` must be `stdio` or `http`. Timeouts must be positive and at most `10m`. |
| Tools missing, quarantined count above 0 | The server sent a bad tool schema or hit a size limit. Check the server's own logs. One bad tool never blocks the rest. |
| Edit did not apply | Restart the service. MCP has no live reload. `config set` reminds you with `{"saved":true,"restart_required":true}`. |

Useful commands:

```sh
q-agent config get mcp.servers.demo.transport
q-agent config show --json | grep -i mcp
q-agent config check
journalctl --user -u q-agent -f   # watch the per-task mcp: line
```

## 8. What is not supported

Standalone resources and prompts, roots, sampling, elicitation, OAuth or
stored credentials, custom headers, secret injection into environments,
legacy HTTP+SSE transport, and bundled server binaries. q-agent calls
`tools/list` and `tools/call` over stdio or Streamable HTTP only.
