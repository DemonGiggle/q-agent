# q-agent — Configuration & Operations

[← Back to the overview](../README.md)

A standalone Telegram agent derived from qcode commit `e9389fa`. Tools execute
directly on the Linux host by default. Each authorized chat receives a persistent
working folder; topics share that folder and have separate conversations.
An isolated Debian trixie filesystem is available through explicit sandbox opt-in.
qcode's MIT notice is retained in
[LICENSE](../LICENSE) and [NOTICE](../NOTICE).

The host process owns provider connections, credentials, polling, conversation
history, and completed replies. The six local tools use the selected execution
mode. V1 supports `openai`, `ollama`, and `opencode-go`, including
the pinned qcode protocol routing and reasoning replay behavior.

## Setup

For guided model/settings menus or prompt-free commands for automation, see
the [console guide](console.md). These edit the same TOML file and do not restart
the running service.

Use Linux and a service account with the permissions you want tools to have.
Host startup requires no image build, bubblewrap, mapping helpers, or subordinate
ID allocations. Build a static native binary with Go 1.22 or newer:

```sh
make build
cp config.example.toml config.toml
```

Edit `config.toml`: set the model, approved Telegram user IDs, and approved chat
IDs. **Both allowlists must match every prompt and command.** For a private chat,
the chat ID normally equals the user's ID; group chat IDs are usually negative.
Configuration uses TOML, rejects unknown fields, and resolves relative paths
against the configuration file's directory. Credentials are environment values,
not configuration values.

Export credentials and start polling:

```sh
export TELEGRAM_BOT_TOKEN='your BotFather token'
./bin/q-agent serve --config config.toml
```

Alternatively, set `telegram.token_path` to a file containing the bot token
and clear `token_env`; the two settings are mutually exclusive. The token
file follows the same strict rules as the provider key file.

`sandbox.enabled` defaults to `false`. Omitting it selects host execution even
when other sandbox settings are present. Only the TOML boolean `true` enables
sandbox execution; strings and other types are rejected.

For OpenAI, set `provider.name` to `openai`, choose the model, remove the Ollama
`base_url` or set the desired compatible URL, and export `OPENAI_API_KEY`. For
OpenCode Go, use `opencode-go`, remove the Ollama URL, and export
`OPENCODE_GO_API_KEY`. `api_key_env` can override the credential variable.
Alternatively, set `provider.api_key_path` to a file containing the key and
clear `api_key_env`; the two settings are mutually exclusive. The key file
must be a regular file owned by the service user with mode `0600` or `0400`,
must not be a symlink, and is limited to 64 KiB; surrounding whitespace is
trimmed. Set `provider.thinking` to a supported effort level when desired. Wire routing stays
pinned to qcode's catalog; adding future protocol routes requires an adapter
update. The providers share HTTP connections while each conversation retains its
own persisted OpenCode session identity. `/new` discards that identity.

For groups, disable privacy mode through BotFather (`/setprivacy` → Disable),
then add or re-add the bot as instructed by Telegram. The adapter accepts mentions,
replies to the bot, and `/ask@your_bot prompt`; it ignores unaddressed messages and
commands directed to another bot. Private chats accept text directly. See
[Telegram's privacy rules](https://core.telegram.org/bots/faq#what-messages-will-my-bot-get).
Replies and typing indicators retain the incoming topic ID. Replies are plain
text, split at a conservative 4096 UTF-16 units without splitting Unicode code
points; attachments and streaming replies are outside v1.

### Telegram commands

- `/status`: read-only snapshot of provider, model, thinking setting, host/sandbox
  mode, this topic's activity, global task concurrency, scheduler lifecycle,
  and step/tool/timeout limits. It reports configured routing, not a provider
  health check or the model's resolved thinking behavior. It works while busy,
  without calling the model, provisioning a workspace, or changing history.
- `/models`: browse the active provider's models with inline buttons (eight per
  page, previous/next navigation). The current model is marked with a check.
  Only the approved user who opened the menu can use it, in the original
  chat/topic. Menus expire after 15 minutes or a service restart; opening another
  menu in the same topic replaces your old one. Discovery failures are reported
  without exposing provider diagnostics.
  Selecting a model saves a **chat-wide** override (all topics), effective for
  the next task, including scheduled agent tasks. Other chats and the global
  config are unchanged. The choice survives restarts and `/new`; `/status`
  reports the effective chat model. Changing the configured provider makes
  overrides from the old provider inactive. Thinking settings remain configured
  globally; provider discovery does not guarantee model access or compatibility.
  Switching is rejected while any task in this chat is active. A successful
  switch invalidates all existing menus in the chat and starts fresh topic
  history lazily on the next task, including when switching back to an earlier
  model. Choosing the already-active model retains history. Workspace files and
  schedules are retained. Callback selection is durable, but a crash before its
  acknowledgement may require reopening `/models` to confirm.
- `/help`: list available commands without calling the model.
- `/new`: clear this topic's conversation, retaining its workspace.
- `/cancel`: request cancellation of this topic's active task.
- `/ask <prompt>`: submit a prompt (plain text also works in private chats).

All commands require an approved user and chat. In groups, use
`/status@your_bot`, `/status @your_bot`, or reply to the bot with `/status`;
unaddressed commands and commands for another bot are ignored. Replies preserve
the original chat/topic and use the durable outbox, including initial model
keyboards and selection confirmations. Page edits and callback acknowledgements
are best-effort Telegram requests, not durable deliveries. Status deliberately excludes
credentials, credential sources/paths, provider URLs, filesystem paths, allowlists,
and other conversations' content. It does not test provider/network health.

### Sandbox opt-in

Set the execution mode explicitly:

```toml
[sandbox]
enabled = true
base_image = "~/.local/share/q-agent/images/debian-trixie.tar"
```

Sandbox mode requires unprivileged user, mount, PID, IPC, UTS, and cgroup
namespaces, a non-root service account, bubblewrap, `newuidmap`, `newgidmap`, and
mmdebstrap. On Debian, install `bubblewrap`, `uidmap`, `mmdebstrap`, and
`debian-archive-keyring`. Each of `/etc/subuid` and `/etc/subgid` must allocate at
least 65,536 IDs to the service account, for example `agent:100000:65536`.

Build the cached base image once before serving in sandbox mode:

```sh
./bin/q-agent image build --config config.toml
./bin/q-agent serve --config config.toml
```

The image command runs rootless mmdebstrap with `--mode=unshare`, `--variant=apt`,
`--include=ca-certificates`, native architecture, and the fixed suite `trixie`.
A checksum/architecture manifest marks successful completion; sandbox startup
refuses an absent, incomplete, or mismatched image. An existing valid image is
reused. Sandbox setup failure stops startup or execution and never falls back to
host execution. The `image build` command remains available in either mode.
[mmdebstrap documentation](https://manpages.debian.org/testing/mmdebstrap/mmdebstrap.1.en.html)
explains rootless operation and subordinate IDs.

## Commands and tools

- `/ask@your_bot prompt`: start a request in a group or topic. `/ask prompt` also
  works in private chats and addressed replies.
- `/new` (or `/new@your_bot`): clear the current topic's history, keeping files and
  installed packages. An active conversation must finish or be cancelled first.
- `/cancel` (or `/cancel@your_bot`): cancel this conversation's active request.
  Changes already made to its filesystem persist.

The model can call `read`, `list`, `search`, `write`, `edit`, and `shell`, plus
the host-managed `schedule_create`, `schedule_list`, and `schedule_delete` tools. In host
mode, relative paths resolve under the chat's persistent folder and absolute
paths refer to the host filesystem. In sandbox mode, absolute paths refer to the
sandbox root and relative paths resolve under `/workspace`. The system prompt
and tool descriptions identify the active mode and working directory.
`edit` requires one unique matching span. `search` uses Go regular expressions,
walks regular files without following symlinks, and skips files over 2 MiB.
File edits are limited to 8 MiB. Tool output is capped at 8 KiB, including an
explicit `[output truncated]` marker. Shell commands use `/bin/sh -c` with an
explicit per-command working directory and environment. In sandbox mode,
ordinary `apt-get update && apt-get install -y ...` installations persist.

## Isolation and state

Host tools run as the service account with ordinary host permissions. Chat folders
organize files; absolute paths and `..` can access other host files allowed by
those permissions. Shells inherit the service environment except configured
Telegram/provider credential variables, the built-in credential-variable list,
and names containing `TOKEN`, `SECRET`, `PASSWORD`, `PASSWD`, `CREDENTIAL`,
`API_KEY`, or `APIKEY` (case-insensitively). Removed values join the redaction
policy. This filtering does not prevent access to credential files or other host
state accessible to the service account.

Host folders and their permanent `(platform, chat ID)` ownership records are
published together through a private staging directory and an atomic rename.
Failed or interrupted provisioning does not start agent work. Each shell retains
bounded output, timeouts, cancellation, and process-group cleanup. Ordinary
background children are killed on completion; host mode cannot guarantee the
termination of children that deliberately detach from the command's process group.

In sandbox mode, provisioning extracts an independent rootfs into a private
staging directory, preserving numeric ownership and modes. Only completed provisioning publishes
the rootfs and its immutable `(platform, chat ID)` owner record. Failed or
interrupted provisioning never starts model or tool work. Abandoned
`.provision-*` directories after a host crash remain unpublished.

Every sandbox tool launch performs bubblewrap's `--userns-block-fd` handshake
with the mapping helpers and verifies the installed maps before releasing the child:

```text
sandbox 0–65535  → service account's first allocated subordinate range
sandbox 65536   → host service UID/GID
```

The static trusted worker changes to sandbox UID/GID 0 before execution. The
sandbox mounts only its rootfs, a read-only worker binary, fresh `/proc` and
`/dev`, and temporary `/tmp` and `/run`. Its clean environment sets `PATH`,
`HOME=/home/agent`, `LANG=C.UTF-8`, and noninteractive package installation.
It retains only CHOWN, DAC_OVERRIDE, FOWNER, FSETID, SETUID, SETGID, and SYS_CHROOT
inside the user namespace. Namespace or mapping failure stops execution.
PID isolation ends background processes when the tool finishes, including on
cancellation. Networking deliberately uses the host network namespace, so local
network services are reachable. It is filesystem isolation, with networking
available to approved users. Tool-result envelopes mark observations as untrusted
reference data. Known credential values and qcode's credential patterns are
redacted from replies, persisted text, and diagnostics.

Host state is stored under `state_dir`, outside rootfs mounts:

```text
images/debian-trixie.tar[.json]          cached base and manifest (default)
host-workspaces/<chat-hash>/owner.json   permanent host-folder ownership
host-workspaces/<chat-hash>/workspace/   persistent host working folder
workspaces/<chat-hash>/owner.json        permanent ownership
workspaces/<chat-hash>/rootfs/           independently writable Debian tree
conversations/<conversation-hash>.json  provider identity and bounded text pairs
cursor.json                            last accepted Telegram update ID
outbox.json                            completed interactive replies and delivery progress
schedules.json                         schedules, durable runs, scheduled reply progress
mapping.json                           permanent service/subordinate mapping
execution.json                         last execution mode
serve.lock                             exclusive process lock
```

Writes to JSON state use temporary files, atomic renames, and filesystem sync.
On an execution-mode change, startup clears every topic's history and provider
identity before polling or delivering replies. Files, the polling cursor, and
queued replies are preserved. Missing execution metadata from older installations
means sandbox mode. Startup records the new mode only after clearing succeeds;
a failed or interrupted reset stops startup and retries on the next launch.
Same-mode restarts preserve history. Host folders and sandbox rootfs trees remain
separate, and files are never migrated between modes.

Update IDs are committed before agent work; duplicates and interrupted tasks do
not execute again after restart. Completed replies retry through the outbox and
resume at the last committed chunk. A lost network acknowledgement can duplicate
a reply chunk; it cannot rerun the task. Interactive tasks still have no durable task queue; scheduled tasks use the
durable run queue described below.

Defaults are two concurrent requests across different chats, one request per
chat including provisioning, eight model steps, sixteen tool calls, a five-minute
request deadline, and a two-minute shell timeout. Additional requests receive a
busy reply. History keeps at most twelve user/final-answer pairs and 32 KiB of
serialized history; whole oldest pairs are removed, including an oversized newest
pair if necessary. Tool exchanges and reasoning stay inside the active run.
Changing a conversation's configured provider or model starts fresh history.

Back up the state directory preserving ownership and modes. For sandbox trees,
also preserve subordinate ownership and keep the service account and subordinate
allocations unchanged when restoring. Updating the base image affects future
sandbox workspaces; existing rootfs changes remain in place. Runtime requires Linux; the core, provider interfaces,
normalized messages, and HTTP adapter compile independently of that requirement.
LINE, UI, multi-agent orchestration, skills, learning, web tools, attachments,
streaming replies, distributed jobs, and container orchestration are deferred.

## Persistent schedules

Ask the agent in an authorized chat/topic, for example:

- “Remind me to pick up my keyboard today at 12:30, Asia/Taipei.”
- “Every day at 09:00 Asia/Taipei, inspect this repo and summarize changes.”
- “List my schedules” or “Delete schedule <id>.”

The agent uses host-managed tools (also available in sandbox mode):

- `schedule_create`: `kind` (`reminder` or `agent`), `text` (up to 8192 bytes),
  `at` (first occurrence, a future RFC3339 timestamp with explicit UTC offset),
  `repeat` (`once`, `daily`, `weekly`), and `timezone` (IANA name).
- `schedule_list`: this user's schedules in this topic (10 per page, optional
  `offset`, follow `next_offset` until -1) and the last 5 retained runs, including
  statuses and reply delivery cursors. Text is previewed to keep output bounded.
- `schedule_delete`: remove an owned schedule by `id`, cancelling pending runs.
  Already running work is unaffected; `/cancel` in its topic stops it. `/new`
  clears conversation history, not schedules. Delete completed one-time
  schedules when no longer needed.

Schedules retain their creating user, chat, and topic. Tool callers cannot
supply another destination or inspect/delete another user's schedules.
Authorization is checked again before execution. Revoked pending tasks become
`cancelled` without sending a notification. Scheduled work uses the current
provider, execution mode, and conversation history at execution time. Pure
reminders bypass the model and tools entirely; agent jobs share provisioning,
workspace exclusion (one task per chat across topics), global concurrency limits,
timeouts, history, and cancellation with interactive requests. Busy scheduled
jobs remain queued; interactive requests retain the existing Busy behavior.

A single goroutine scans immediately on startup and on each wall-clock minute
boundary. Expected scan lateness is 0–60 seconds, plus any worker or network
wait. A task finishing wakes the dispatcher so queued work need not wait for the
next minute. Only occurrences up to five minutes late are admitted after missed
scans/restarts. Daily/weekly downtime is coalesced to the latest due occurrence;
older occurrences are not replayed. Stale/overlapping occurrences are recorded
as `missed`, and the schedule advances from its wall-clock cadence, never from
completion time. One pending/running occurrence per schedule prevents overlap.
Once admitted, a run does not expire while waiting for capacity. Recurrences use
the specified timezone and local wall time; nonexistent DST times are skipped
and ambiguous DST times run once. The first occurrence is exactly `at`; the next
weekly occurrence uses its local weekday.

Schedules and runs live in one atomically replaced, synced `schedules.json`.
Advancing `next_run_at` and enqueuing the unique `(schedule ID, scheduled time)`
run are a single commit. Channels only wake dispatch; they never hold the sole
copy of a task. Successful repeated identical create calls return the same
schedule. The store permits 100 schedules per conversation, applies backpressure
at 1000 outstanding runs/replies, and retains up to 1000 fully delivered
terminal run records globally. Pending work and undelivered output are never pruned.
At capacity, unadmitted schedules remain due; the normal grace policy applies
when capacity returns. Missed counters count recorded/coalesced occurrences,
not every historical recurrence during a long outage.

Workers persist `running` before agent work. A crash at that point has unknown
side effects: startup marks it `interrupted`, queues a notification, and **does
not automatically retry it**. Failed/timed-out agent runs also do not retry.
Pending runs survive restart. A completed run and its reply chunks are committed
in the same record; scheduled reply IDs are independent of Telegram UpdateIDs.
Delivery resumes from its saved chunk, alternating ordinary and scheduled output
so one failed destination does not block the other queue. Network acknowledgement
ambiguity can still duplicate a reply chunk. This is not an exactly-once guarantee
for external side effects. State write errors are reported to the service log;
if an agent's final commit fails, its persisted running record is conservatively
recovered as interrupted on restart, not executed again.

No Redis, database service, OS cron, or additional Go dependency is required.
This is a bounded, single-process scheduler, protected by the existing serve
lock; arbitrary cron expressions, automatic retries, second-level timing, and
multi-process workers are outside this version.

## Verification

```sh
make test
make race
go vet ./...
make build
make integration-host
Q_AGENT_IMAGE="$HOME/.local/share/q-agent/images/debian-trixie.tar" make integration
```

Host tests exercise all six tools, credentials, absolute host paths, concurrent
chat folders, shared topics, restart persistence, output bounds, process cleanup,
timeouts, and cancellation. A mocked Telegram/Ollama service test uses real host
tools with deliberately missing sandbox dependencies and images, and runs in the
ordinary unit suite. State tests cover both mode transitions, legacy sandbox
history, same-mode restarts, and failed or interrupted resets.

Sandbox integration tests explicitly enable sandbox execution and use a real
static worker and two separate rootfs directories.
They verify mappings/capabilities, inaccessible host files, clean credentials,
file persistence, bounded output, process termination, cancellation, shell timeout,
failed mapping and provisioning, and installing `jq` only in one workspace with
persistence after reopening it. A sandbox service test uses mocked Telegram and
Ollama endpoints with a real sandbox; it checks polling, authorization, tool
continuation, topic replies, and host state. These tests send no live Telegram
messages. Test-created rootfs directories are removed through the mapped worker.
See [VALIDATION.md](../VALIDATION.md) for measured results and limitations.

### GitHub Actions CI

The `CI` workflow runs on pull requests targeting `main`, pushes to `main`, and
manual dispatch. Checks cover the minimum Go version (1.22) and the latest stable
Go release: formatting, uncached tests (including the mocked host service
integration), vet, and the static Linux build. A separate Linux amd64 job runs
uncached race tests with the latest stable Go release and CGO enabled.

CI needs no Telegram/provider credentials and makes no live model requests.
The opt-in rootless sandbox integration (`make integration`) is not included:
it requires a Debian image, namespace support, and UID/GID mapping helpers.
The workflow only validates code; it does not publish binaries or deploy.
Required-check branch protection must be configured separately in GitHub if
merges should be blocked by failing CI.
