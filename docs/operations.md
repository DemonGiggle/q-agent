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

Requires Linux, Go 1.27+ to build, and a service account with whatever
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

On load, `serve` and the `models`/`config`/`login` console commands
automatically migrate deprecated keys and rewrite the file in place,
preserving comments and permissions. Handled so far: `max_tool_calls`
removal (no longer used) and the old `approved_user_ids` /
`approved_chat_ids` allowlist (rewritten as `allowed_users` /
`allowed_groups`). A timestamped backup (`config.toml.bak.YYYYMMDD-HHMMSS`)
is kept next to the file, and the migration is reported on stderr
(`[SERVICE] Migrated config ...` for `serve`). Files that mix old and new
allowlist keys are left untouched with an error so you can resolve the
conflict explicitly; read-only files keep working from the in-memory
migration for the allowlist case.

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

```text
TELEGRAM_BOT_TOKEN=[REDACTED]
OPENAI_API_KEY=sk-...
```

```sh
chmod 0600 ~/.config/q-agent/service.env
```

### Providers and models

`[provider]` is the global default. Add named tables to offer more choices:

```toml
[providers.openai]
model = "YOUR_OPENAI_MODEL"
api_key_env = "OPENAI_API_KEY"
```

Supported names: `ollama`, `openai`, `opencode-go`, `chatgpt`. Each needs a model and
can set its own `base_url`, credentials, and `thinking`. Only the operator
configures these; Telegram users can never supply endpoints or keys.

Manage them without hand-editing TOML:

```sh
q-agent models set-default --provider openai --model YOUR_MODEL_ID
q-agent models add --provider openai --model YOUR_MODEL_ID --api-key-env OPENAI_API_KEY
q-agent models update --provider openai --model NEW_MODEL
q-agent models remove --provider openai
q-agent models show --all --json
```

`set-default` edits only `[provider]`; `add` creates an extra without replacing
the default. `models set` remains as a deprecated alias for `set-default`.

Per-chat overrides (`/provider`, `/model`, `/think`, `/max_steps`) change
only the current private chat or group, survive restarts and `/new`, and
apply to scheduled tasks. Switching provider or model starts fresh topic
history on the next task; thinking-only changes keep history.
Changes are rejected while that chat has an active task.

Thinking semantics (`thinking` in config, `/think` in chat):
`auto` (empty string) omits the effort field and lets the backend decide; it
never claims equality with a concrete level. `/think default` clears the chat
override and restores the configured default (which may itself be explicit).
For the `chatgpt` provider the live Codex catalog carries per-model
`default_reasoning_level` / `supported_reasoning_levels`; the Codex CLI sends
its catalog default explicitly when unset while q-agent omits effort when
`auto` — bare `/think` shows `Current`, `Chat override`, `Configured default`,
and `Codex catalog default` on separate lines plus the catalog
`source`/`stale` status. `ultra`/`persistent`/`custom` are client-only in v1
and are rejected, never silently downgraded. Reasoning `summary` is omitted by
default; effort and summary are independent. Models.dev is not used for
`chatgpt` (see `model_meta.go`); the Codex `/models` catalog is authoritative.

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
places (sandbox mode). Only sandbox mode isolates chats: each chat gets
its own `rootfs` tree. In host mode the per-chat folders are organizational
only — tools run as the same service account with no path containment, so
an absolute path or `..` can reach another chat's files.
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
| `/provider`, `/provider <name\|default>` | Button picker for this chat's provider, or same as text command. Provider switch clears the model override. |
| `/model <id\|default>` | Same as above as text command. |
| `/think <level\|auto\|default>` | Thinking override for this chat. |
| `/max_steps [n\|default]`, `/settings [reset]` | Step limit / show or clear all overrides. |
| `/memory`, `/forget` | Show memory state / clear notes + pending sources for the whole chat. Files, history, schedules stay. |
| `/companion` | Quiet check-ins for this chat: on by default, `/companion` shows buttons, `/companion on\|off\|default\|status` as text. Chat-wide. |
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
`🔧/✅/⚠️` tool counts, then streamed answer text in batches at paragraph
boundaries or every few seconds). Rapid status changes are coalesced. It is
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

### Companion: quiet check-ins, on by default

- `[proactive]` runs a decoupled sweep at random moments inside `active_time` (default 09:00-22:00 server-local), a few times per day. Each authorized chat is visited at most once per round. Silence stays a healthy outcome, but casual small talk and idle chatter should usually earn a `warmth` check-in instead of silence.
- Input is read-only: addressed history (last 6 pairs per topic) plus memory notes plus live group chatter inside `recent_window` (default 12h, all participants even without a mention). One cheap no-tools model call (temperature 0.7) decides silent, reminder, warmth, comfort, or suggestion; idle chatter is an explicit warmth trigger, only genuinely empty input, focused ongoing tasks, uncertain reminder timing, or repeats stay silent. At most 1 short text per chat per sweep, capped at max_per_day 1 and 6h cooldown, sent via a dedicated durable companion outbox with redaction. Queueing, cooldown evidence, and the outgoing draft commit atomically, with unique IDs across chats; queued is not the same as delivered. The daily cap is a rolling 24-hour limit and survives restarts. Sweep spacing inside the window is randomized at roughly half to most of cooldown, so longer cooldowns naturally sweep less often. No tool calls, shell, web, or cross-chat profiling in v1. Replies go to the workspace route (topic 0). Delivered check-ins are included as bounded reference context for follow-up conversation in topic 0 (not other topics). The decider receives the current time and recent check-ins, and is told not to infer deadlines from undated messages. Unchanged conversation/memory and identical drafts do not generate another check-in, even after cooldown. This is lightweight companionship, not a reliable reminder or task-execution service.
- Safety: skips unauthorized chats, busy tasks, shell prompts, maintenance, recent activity (default 60m, addressed interaction only; taps do not update LastSeen), and empty history+memory+taps. Groups default to conservative (reminders only); group_policy off skips groups and skips tap collection. Private taps are skipped (private history is already complete).
- Controls: global `[proactive] enabled=false` is the kill switch (no goroutine, no background history reads or LLM calls). Per-chat `/companion`, `/companion on|off|default|status|history` has buttons and today’s diagnostics. Default follows global; off opts this chat out; on opts in only while the global kill switch is enabled. A saved on preference never bypasses the global kill switch. Anyone in an allowed group may change that group. Upgrade note: installs without a `[proactive]` section auto-enable on next load.

### Companion diagnostics

Opening `/companion` shows today’s actual per-chat sweep start times within
`active_time`, using `[proactive] timezone` (server-local when omitted), alongside
the on/off controls. `/companion history` and `/companion status` refresh the
same view without a model call. For an overnight window, “today” means the
current local calendar date, including starts after midnight.

Results distinguish skipped (with a fixed reason), successful silence (with the
decider's short one-line reason, e.g. `OK · stayed silent · model chose silence: <reason>`),
model failure (with a short machine reason, never raw provider errors), queued
(delivery unconfirmed), delivered, failed delivery awaiting retry, and a sweep
interrupted by service restart. Full drafts, raw provider errors, and other
chats’ records are not shown in diagnostics.
At most the latest 20 retained starts are displayed. No records does not mean
failure: startup waits a randomized 50–90% of cooldown before an in-window
sweep, and the global kill switch prevents sweeps entirely. Viewing the menu
counts as recent chat activity, so the next sweep may be skipped for that reason.

State lives in `companion_activity.json` plus `recent.json` (live taps). Taps are per-chat capped (default 200 msgs / 32 KiB, oldest evicted), expire after `recent_window`, and are consumed up to the sweep start watermark after a terminal evaluated sweep (silent, queued, kind-disabled, group-conservative, empty/oversized draft, duplicate). Early skips (busy, rate-limited, recent-activity, races) and failures keep taps for the next sweep; expiry pruning runs on every sweep that reaches input, so disk stays bounded even when unprocessed. `enabled=false` stops tap collection entirely. Ordinary diagnostic retention is
bounded to 128 starts / seven days per chat; pending delivery, the latest
check-in, and rolling 24-hour send evidence are protected from pruning.
Existing installations have no historical records until their first new sweep.
Defaults remain low frequency: one message per rolling 24 hours, six-hour
cooldown, no tools, and silence preferred. No credentials or config migrations
are required.

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

Interactive sessions (both modes): `shell` with `pty:true` (terminal) or
`background:true` (pipe) starts a task-scoped session returning `session_id`
and initial output after `yield_ms` (default 1000 ms, 0–60000). Ordinary
synchronous shells keep their timeout behavior. The `process` tool drives
sessions: `list`, `read` (including `view:"screen"`), `write`, `keys`,
`resize`, `wait`, `terminate`. See below.

### Interactive process sessions

Use ordinary Telegram messages to ask the agent to operate an interactive app.
The agent can start a terminal with
`shell {"command":"gh auth login","pty":true}` or a pipe session with
`shell {"command":"python3 -m http.server 8080","background":true}`.
Either option returns a task-scoped `session_id` and initial output after
`yield_ms`. Ordinary shell calls retain their existing synchronous timeout
behavior.

The `process` tool supports these actions:

- `list`: list the current task's sessions and exit status.
- `read`: read history from `cursor` (default 0), continuing at `next_cursor`.
  `history_start` identifies dropped older bytes; `truncated` also indicates
  another page is available. Cursors refer to redacted UTF-8 history bytes.
- `read` with `view:"screen"`: read the current PTY screen, its dimensions,
  cursor, alternate-screen state, and text emphasis/colors. Positions are
  zero-based. Large screens continue at `screen.next_offset` using `offset`.
- `write`: send exact `data` to stdin. Include a newline for pipe input.
- `keys`: send named keys such as `Enter`, `Up`, `Down`, `Escape`, `Ctrl-C`,
  or `Ctrl-D`. Ctrl-D provides terminal EOF; it does not close a pipe.
- `resize`: set `rows` (1–120) and `columns` (2–240) on a PTY.
- `wait`: wait for output, exit, or an ordinary follow-up from the task owner.
  `timeout_ms` defaults to 30000, at most 60000. Supply the last `next_cursor`
  to wait for new output. An optional `message` posts an interim reply before
  waiting; `human_input:true` marks a request for a human response.
- `terminate`: stop and reap the session and its owned process group.

Terminals default to 80×24 and answer cursor/device queries used by terminal
applications. A task can keep two sessions at once; completed records are
replaced when necessary to retain at most two. Duplicate commands already
running in that task are rejected. Each session retains 256 KiB of history;
each encoded process result is at most 8 KiB. Redaction runs before pagination,
strips terminal decoration for history detection, and covers wrapped and
fragmented known credentials. Raw screen/history data stays in bounded memory.

Sessions survive tool calls while the task is active. A final answer, failure,
step limit, cancellation, deadline or shutdown closes them. Later tasks cannot
reuse their IDs. On the first managed launch,
`limits.interactive_task_timeout` (default `15m`) becomes the total task budget
measured from task start; a longer `limits.task_timeout` is preserved. Further
launches do not renew the budget. Sandbox sessions keep one validated worker
alive per session and allocate PTYs in bubblewrap's private `/dev` and `devpts`.
Host cleanup retains the documented limitation for deliberately detached
processes. Servers intended to run beyond the task need an explicitly requested
persistent service, rather than a process session.

Browser/device authorization URLs and short authorization codes may be sent in
an interim message. Reply in the same Telegram topic to steer the active agent;
it decides what input to send to the app. Do not send passwords or long-lived
tokens in chat: use preconfigured credentials or browser authorization.
`/status` and `/cancel` continue to work during waits. Scheduled tasks may drive
applications automatically, but `human_input:true` returns an immediate,
recoverable `human input unavailable` observation.

Photos: send a photo with a caption (`@your_bot` in groups) as model input.
Needs a vision-capable model; up to 10 MB per image, binary never stored in
history.

Files: send a document, video, audio, voice, or animation with a caption saying
what to do. Files are saved to `inbox/` in the chat workspace and persist
across tasks; reference them as `inbox/<name>` or `/workspace/inbox/<name>`.
Up to 5 files per message, 20 MB each, 100 MB per chat inbox. A daily sweep
removes files older than 24h (busy chats are skipped to the next day), so act
promptly. Image documents
double as vision input. Albums arrive as separate messages.

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
CI (Go 1.27 + stable, fmt, tests, vet, static build, amd64 race) validates
code only — it never publishes or deploys. The opt-in rootless sandbox
integration is not in CI.

## 7. Testing

```sh
make test                 # unit suite (includes mocked host service test)
make race                 # race detector
go vet ./...
make build
make integration-host     # host-mode service test
Q_AGENT_IMAGE="$HOME/.local/share/q-agent/images/debian-trixie.tar" make integration
```

Sandbox integration uses a real static worker and isolated rootfs trees.
It sends no live Telegram messages and makes no paid model calls.
See [VALIDATION.md](../VALIDATION.md) for measured results.
Systemd-specific probes are in the [systemd guide](systemd.md#validation).
