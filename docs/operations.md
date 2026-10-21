# q-agent — Configuration & Operations

[← Overview](../README.md) · [systemd deployment](systemd.md) · [Console guide](console.md)

A standalone Telegram agent. Tools run on your Linux host by default;
an isolated Debian filesystem is available through explicit sandbox opt-in.

One rule explains most of this file:

> **The host owns everything sensitive.** Provider connections, API keys,
> polling, conversation history, and replies live in the host process.
> Tool calls run either directly on the host or inside a sandbox worker —
> the sandbox never sees your keys.

## 1. Setup

Requires Linux, Go 1.22+ to build, and a service account with whatever
permissions you want your tools to have. Host startup needs no image,
bubblewrap, or subordinate IDs.

```sh
make build
mkdir -p ~/.config/q-agent
cp config.example.toml ~/.config/q-agent/config.toml
```

All commands use `~/.config/q-agent/config.toml` by default.
Use `--config FILE` to select another file. The file uses TOML, rejects
unknown fields, and resolves relative paths against its own directory.

### Allowlist: who can talk to the bot

```toml
[telegram]
allowed_users = [123456789]      # private chats: your user ID (chat ID == user ID)
allowed_groups = [-1001234567890] # groups/channels: anyone inside can talk
```

- A private chat is allowed when its ID is in `allowed_users`.
- A group message is allowed when its chat ID is in `allowed_groups`,
  no per-user listing needed. In groups, disable privacy via BotFather
  (`/setprivacy` → Disable), then mention the bot or reply to it.
- Rejected messages are dropped silently (check the journal for
  `rejected (not approved)` to discover IDs).

### Credentials: env var or file, never in TOML

```sh
export TELEGRAM_BOT_TOKEN='[REDACTED]'
export OPENAI_API_KEY='sk-...'        # or OPENCODE_GO_API_KEY, or nothing for ollama
./bin/q-agent serve
```

Alternatively set `telegram.token_path` / `provider.api_key_path` to a file
and clear the corresponding `*_env`. The two sources are mutually exclusive.

File rules (both token and provider key): regular file (no symlink), owned
by the service user, mode `0600` or `0400`, max 64 KiB, surrounding
whitespace trimmed. Startup refuses to run if the rules are violated
(except ollama, which allows an empty key).

Example `service.env` next to the config (systemd only, not a shell script —
no `export`, no substitution):

```sh
TELEGRAM_BOT_TOKEN=[REDACTED]
OPENAI_API_KEY=sk-...
chmod 0600 service.env
```

### Providers and models

`[provider]` is the global default. Add named tables to offer more choices:

```toml
[providers.openai]
model = "YOUR_OPENAI_MODEL"
api_key_env = "OPENAI_API_KEY"
```

Supported names: `ollama`, `openai`, `opencode-go`. Each needs a model and
can set its own `base_url`, credentials, and `thinking`. Only the operator
configures these; Telegram users can never supply endpoints or keys.

Per-chat overrides (`/provider`, `/model`, `/think`, `/max_steps`) change
only the current private chat or group, survive restarts and `/new`, and
apply to scheduled tasks. Switching provider or model starts fresh topic
history on the next task; thinking-only changes keep history.
Changes are rejected while that chat has an active task.

For guided edits or scripts, use the [console guide](console.md)
(`q-agent models`, `q-agent config`). Edits never restart the service.

## 2. How tools run: host vs sandbox

This is the section people get wrong, so here it is in one place.

**Host mode** (`[sandbox] enabled = false`, the default) runs tools directly
as the service account via `localtools.Execute`. Fast, no isolation.

**Sandbox mode** (`enabled = true`) runs each tool call inside bubblewrap.
The host spawns the sandbox and the sandbox reports back:

```text
host:  bwrap --bind <rootfs> / --ro-bind <q-agent-binary> /.q-agent-worker ...
       /.q-agent-worker worker tool
       stdin  {"Call": {...}, "ShellTimeout": ...}   → sandbox
       stdout {"Output": "...", "Error": "..."}      → host
```

There is no separate `q-agent-worker` program. It is the same `q-agent`
binary, bound read-only at `/.q-agent-worker` and dispatched via
`q-agent worker tool` (also `worker extract` / `worker remove` for image
setup). Inside, it checks `Q_AGENT_WORKER=1`, UID/GID `65536`, and the
user-namespace maps before switching to root, and it must be a static
binary (`CGO_ENABLED=0`). Directly running `q-agent worker tool` outside
the sandbox is refused.

### Where your files actually are

| Inside the sandbox you see | On the host disk it is | Survives restart? |
| --- | --- | --- |
| `/workspace/...` | `state_dir/workspaces/<chat-hash>/rootfs/workspace/...` | Yes |
| `/home/agent`, installed packages, `git clone` results | Same `rootfs/` tree | Yes |
| `/tmp`, `/run` | tmpfs, fresh every tool call | No |

| In host mode you use | On the host disk it is | Survives restart? |
| --- | --- | --- |
| `./relative/path` | `state_dir/host-workspaces/<chat-hash>/workspace/...` | Yes |
| `/absolute/path` | That exact host path (service-account permissions) | Depends on the path |

`git clone` therefore persists in both modes — it lands in the chat's
persistent folder above. `/tmp` and `/run` are the only truly temporary
places (sandbox mode). Different chats never see each other's folders.
Switching modes does **not** migrate files: each mode keeps its own tree,
and switching clears conversation history (files, cursor, and queued
replies are kept). Sandbox failures never fall back to host mode.

### What the sandbox can and cannot see

- Clean environment only: `PATH`, `HOME=/home/agent`, `LANG=C.UTF-8`,
  `Q_AGENT_WORKER=1`, `DEBIAN_FRONTEND=noninteractive`. Everything else is
  dropped by `--clearenv`.
- Credential variables (`TELEGRAM_BOT_TOKEN`, your configured
  `api_key_env`, plus anything containing `TOKEN`, `SECRET`, `PASSWORD`,
  `PASSWD`, `CREDENTIAL`, `API_KEY`, `APIKEY`) are stripped by
  `FilterEnvironment` before the sandbox starts, and their values join the
  log redaction list. Host mode applies the same stripping to tool shells
  (it does not block reads of credential *files* the account can read —
  restrict the account instead).
- API keys are resolved once at startup into host memory and sent as
  `Authorization: Bearer ...` on every provider request. The sandbox only
  ever receives the ToolCall JSON, so it cannot leak a key.
- Mounts: the chat's `rootfs`, a read-only worker binary, fresh `/proc`
  and `/dev`, temporary `/tmp` and `/run`. Config, credentials, and all
  of `state_dir` stay outside. PID isolation ends background processes when
  the tool finishes. Networking intentionally uses the host network
  (filesystem isolation, not network isolation).

### Sandbox requirements (both foreground and systemd)

- Non-root service account, kernel user/mount/PID/IPC/UTS/cgroup namespaces.
- On Debian: `bubblewrap`, `uidmap`, `mmdebstrap`, `debian-archive-keyring`.
- ≥ 65,536 subordinate IDs in **both** `/etc/subuid` and `/etc/subgid`.
- A static binary (`make build` qualifies) and a built base image:

```sh
./bin/q-agent image build   # once, creates images/debian-trixie.tar
./bin/q-agent serve
```

Missing image, helpers, allocations, or namespace support stops startup —
never silently runs on the host instead. See the
[systemd guide](systemd.md#sandbox-on-systemd) for the two extra unit rules.

## 3. Talking to the bot

All commands need an allowed private user or allowed group. In groups,
address the bot (`/status@your_bot`, `/status @your_bot`, or reply with
`/status`). Replies stay in the originating chat/topic via a durable outbox.

| Command | Effect |
| --- | --- |
| `/status` | Read-only snapshot: provider, model, activity, memory use, limits. Works while busy, never calls the model. |
| `/models` | Button picker (8 per page) for this chat's model. Chat-wide, takes effect next task, clears topic history. |
| `/provider <name\|default>`, `/model <id\|default>` | Same as above as text commands. Provider switch clears the model override. |
| `/think <level\|auto\|default>` | Thinking override for this chat. |
| `/max_steps [n\|default]`, `/settings [reset]` | Step limit / show or clear all overrides. |
| `/memory`, `/forget` | Show memory state / clear notes + pending sources for the whole chat. Files, history, schedules stay. |
| `/new` | Clear this topic's history. Files and schedules stay. |
| `/cancel` | Stop this topic's task. Finished side effects stay. |
| `/help` | Command list. |
| `/update`, `/restart` | Admin only (`telegram.admin_user_ids`, must also be in `allowed_users`), private chat only, no active tasks. Summary here, details in [systemd guide](systemd.md#restart-and-update). |

Steering a running task: new text from the same sender in the same
chat/topic is merged (up to 16 messages / 64 KiB) into the next model
request — it never undoes a tool side effect. Anything else (other topics,
other users) cannot inject. Pending steering is in-memory only; a restart
drops it — resend after restart. `/status` shows the pending count while busy.

Live progress: one `Working…` preview per task (`contacting <model>`,
`🔧/✅/⚠️` tool counts, then streamed answer text ~1 edit/sec). It is
best-effort; the outbox reply that follows is authoritative and resumes
after restart. Thinking traces never stream.

### Schedules: reminders and agent tasks

Just ask, e.g. “Remind me today at 12:30 Asia/Taipei…” or “Every Monday at
09:00 Asia/Taipei, summarize…”. Also `List my schedules` / `Delete schedule
<id>`. A reminder replays your text; an agent task runs the model with your
chat's current provider, mode, files, and history.

- Times are RFC3339 with explicit offset plus an IANA timezone;
  daily/weekly use local wall time. One scan per minute (0–60s lateness).
- Short outages (≤ 5 min) catch up once; longer ones coalesce to the latest
  occurrence and record the rest as `missed`. No overlap: one pending/run
  per schedule. Interrupted agent runs notify but never auto-retry.
- `schedules.json` holds everything atomically. Caps: 100 schedules per
  conversation, 1000 outstanding runs/replies, 1000 retained terminal runs.
- Busy chats queue; `/cancel` in the schedule's topic stops its run.
  `/new` does not delete schedules.

### Memory: short-term context and nightly notes

- Short-term: at most 12 user/answer pairs and 32 KiB per topic. A failed
  task saves a bounded (16 KiB) interrupted entry — original request,
  steering, failure reason, tool outcomes — offered to the next follow-up
  in the same topic. It is reference data, not a replay queue: completed
  calls are never re-dispatched automatically.
- Long-term (`[memory]`, on by default): successful tasks journal a source
  event per chat (shared across topics, tools/reasoning excluded). A daily
  sweep (server-local midnight by default) distills sources into notes
  (default 8 KiB text, 2 KiB injected context, 4 MiB pending journal).
  Overflow evicts oldest-first with counters; `/memory` shows budgets,
  pending age, retries, and loss counts. `/forget` clears everything memory
  related. Corrupt memory files are quarantined aside so one bad chat
  cannot block startup.

## 4. Tools the model can call

Local (six): `read`, `list`, `search` (Go regexp, no symlink following,
skips files > 2 MiB), `write`, `edit` (exactly one unique match), `shell`
(`/bin/sh -c`, per-command directory and timeout). Output capped at 8 KiB
with `[output truncated]`; file edits capped at 8 MiB. Plain `apt-get
install` persists in sandbox mode.

Host shells with output that hit `shell_timeout` reply with the last 3 lines
and wait `shell_prompt_timeout` (default 90s) for `extend` (up to
`shell_max_extends`, default 2) or `kill`. Scheduled runs never prompt.
Sandbox shells terminate on timeout.

Web (two, both modes): `web_search` (DuckDuckGo Lite, 1–10 results, no key)
and `web_fetch` (HTTP/S GET, ≤ 5 redirects, no JS/browser/login, 20s
deadline, TLS verified). In sandbox mode they run inside the worker with
the rootfs DNS/CA certs. Results are untrusted reference data — cite source
URLs. `search` is local files; `web_search` is the web.

Host-managed (three, both modes): `schedule_create`, `schedule_list`,
`schedule_delete` — see §3. The model never handles cron or destinations
directly; schedules always return to their creating chat/topic.

Photos: send a photo with a caption (`@your_bot` in groups) as model input.
Needs a vision-capable model; up to 10 MB per image, binary never stored in
history. Albums and document-uploaded images are not supported.

## 5. State, logs, backups

Under `state_dir` (default `~/.local/share/q-agent/`), outside every
rootfs mount:

```text
images/debian-trixie.tar[.json]          base image + manifest
host-workspaces/<hash>/workspace/        host-mode persistent folder
workspaces/<hash>/rootfs/                sandbox-mode Debian tree
conversations/<hash>.json                provider identity + bounded pairs
cursor.json / outbox.json / schedules.json / mapping.json / execution.json
serve.lock                               exclusive process lock
```

JSON state uses temp-file + atomic rename + fsync. Update IDs commit before
work (no double execution); replies retry from the last committed chunk
(a lost acknowledgement can duplicate a chunk, never rerun the task).

Logs (stderr / journal) look like `[RUN] Agent completed | steps=2 ...`.
They carry IDs, counts, timings, and redacted reasons — never message text,
tool arguments, or shell previews.

Back up `state_dir` preserving ownership and modes. For sandbox trees also
preserve subordinate ownership and keep UID/GID allocations stable across
restore. New base images affect future workspaces only.

## 6. Upgrading

```sh
q-agent version
q-agent update                            # latest stable GitHub Release
```

Only Linux amd64/arm64. Trust is GitHub HTTPS + publisher (no separate
signature step). CLI updates replace the binary in place (symlinks
preserved) and touch nothing else — no config, credentials, state, or image
changes, no restart. Restart via your service manager when ready; when
sandbox mode is on, stop first so workers match the host.

Telegram `/update` is different: it stops the bot, updates from an
independent helper, checks readiness, and rolls back on failure. See
[systemd guide](systemd.md#restart-and-update).

Maintainers publish with `make release VERSION=vX.Y.Z`, tag, and attach
both raw binaries (or the `.tar.gz` packages newer updaters accept).
CI (Go 1.22 + stable, fmt, tests, vet, static build, amd64 race) validates
code only — it never publishes or deploys. The opt-in rootless sandbox
integration is not in CI.

## 7. Testing

```sh
make test                 # unit suite (includes mocked host service test)
make race                 # race detector
go vet ./...
make build
make integration-host     # host-mode service test
Q_AGENT_IMAGE="~/.local/share/q-agent/images/debian-trixie.tar" make integration
```

Sandbox integration uses a real static worker and isolated rootfs trees.
It sends no live Telegram messages and makes no paid model calls.
See [VALIDATION.md](../VALIDATION.md) for measured results.
Systemd-specific probes are in the [systemd guide](systemd.md#validation).
