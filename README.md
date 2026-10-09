# q-agent v1

A standalone Telegram agent derived from qcode commit `e9389fa`. Each authorized
chat receives its own persistent Debian trixie filesystem. Topics share that
filesystem and have separate conversations. qcode's MIT notice is retained in
[LICENSE](LICENSE) and [NOTICE](NOTICE).

The host process owns provider connections, credentials, polling, conversation
history, and completed replies. The six local tools run in a fresh bubblewrap
sandbox for every call. V1 supports `openai`, `ollama`, and `opencode-go`, including
the pinned qcode protocol routing and reasoning replay behavior.

## Setup

Use Linux with unprivileged user, mount, PID, IPC, UTS, and cgroup namespaces,
a non-root service account, bubblewrap, `newuidmap`, `newgidmap`, and mmdebstrap.
On Debian, the prerequisite packages are `bubblewrap`, `uidmap`, `mmdebstrap`, and
`debian-archive-keyring`. Each of `/etc/subuid` and `/etc/subgid` must allocate at
least 65,536 IDs to the service account. A typical allocation looks like:

```text
agent:100000:65536
```

Build a static native binary with Go 1.22 or newer:

```sh
make build
cp config.example.json config.json
```

Edit `config.json`: set the model, approved Telegram user IDs, and approved chat
IDs. **Both allowlists must match every prompt and command.** For a private chat,
the chat ID normally equals the user's ID; group chat IDs are usually negative.
Configuration uses JSON, rejects unknown fields, and resolves relative paths
against the configuration file's directory. Credentials are environment values,
not configuration values.

Build the cached base image once, then start polling:

```sh
./bin/q-agent image build --config config.json
export TELEGRAM_BOT_TOKEN='your BotFather token'
./bin/q-agent serve --config config.json
```

The image command runs rootless mmdebstrap with `--mode=unshare`, `--variant=apt`,
`--include=ca-certificates`, native architecture, and the fixed suite `trixie`.
A checksum/architecture manifest marks successful completion; startup refuses an
absent, incomplete, or mismatched image. An existing valid image is reused.
[mmdebstrap documentation](https://manpages.debian.org/testing/mmdebstrap/mmdebstrap.1.en.html)
explains rootless operation and subordinate IDs.

For OpenAI, set `provider.name` to `openai`, choose the model, remove the Ollama
`base_url` or set the desired compatible URL, and export `OPENAI_API_KEY`. For
OpenCode Go, use `opencode-go`, remove the Ollama URL, and export
`OPENCODE_GO_API_KEY`. `api_key_env` can override the credential variable. Set
`provider.thinking` to a supported effort level when desired. Wire routing stays
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

## Commands and tools

- `/ask@your_bot prompt`: start a request in a group or topic. `/ask prompt` also
  works in private chats and addressed replies.
- `/new` (or `/new@your_bot`): clear the current topic's history, keeping files and
  installed packages. An active conversation must finish or be cancelled first.
- `/cancel` (or `/cancel@your_bot`): cancel this conversation's active request.
  Changes already made to its filesystem persist.

The model can call `read`, `list`, `search`, `write`, `edit`, and `shell`. Absolute
paths refer to the sandbox root; relative paths resolve under `/workspace`.
`edit` requires one unique matching span. `search` uses Go regular expressions,
walks regular files without following symlinks, and skips files over 2 MiB.
File edits are limited to 8 MiB. Tool output is capped at 8 KiB, including an
explicit `[output truncated]` marker. Shell commands use `/bin/sh -c`; ordinary
`apt-get update && apt-get install -y ...` installations persist.

## Isolation and state

Provisioning extracts an independent rootfs into a private staging directory,
preserving numeric ownership and modes. Only completed provisioning publishes
the rootfs and its immutable `(platform, chat ID)` owner record. Failed or
interrupted provisioning never starts model or tool work. Abandoned
`.provision-*` directories after a host crash remain unpublished.

Every tool launch performs bubblewrap's `--userns-block-fd` handshake with the
mapping helpers and verifies the installed maps before releasing the child:

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
workspaces/<chat-hash>/owner.json        permanent ownership
workspaces/<chat-hash>/rootfs/           independently writable Debian tree
conversations/<conversation-hash>.json  provider identity and bounded text pairs
cursor.json                            last accepted Telegram update ID
outbox.json                            completed replies and delivery progress
mapping.json                           permanent service/subordinate mapping
serve.lock                             exclusive process lock
```

Writes to JSON state use temporary files, atomic renames, and filesystem sync.
Update IDs are committed before agent work; duplicates and interrupted tasks do
not execute again after restart. Completed replies retry through the outbox and
resume at the last committed chunk. A lost network acknowledgement can duplicate
a reply chunk; it cannot rerun the task. There is no durable task queue.

Defaults are two concurrent requests across different chats, one request per
chat including provisioning, eight model steps, sixteen tool calls, a five-minute
request deadline, and a two-minute shell timeout. Additional requests receive a
busy reply. History keeps at most twelve user/final-answer pairs and 32 KiB of
serialized history; whole oldest pairs are removed, including an oversized newest
pair if necessary. Tool exchanges and reasoning stay inside the active run.
Changing a conversation's configured provider or model starts fresh history.

Back up the state directory with a tool that preserves its subordinate ownership
and modes. Keep the service account and subordinate allocations unchanged when
restoring. Updating the base image affects future workspaces; existing rootfs
changes remain in place. Runtime requires Linux; the core, provider interfaces,
normalized messages, and HTTP adapter compile independently of that requirement.
LINE, UI, multi-agent orchestration, skills, learning, web tools, attachments,
streaming replies, durable jobs, and container orchestration are deferred.

## Verification

```sh
make test
make race
make build
Q_AGENT_IMAGE="$HOME/.local/share/q-agent/images/debian-trixie.tar" make integration
```

Integration tests use a real static worker and two separate rootfs directories.
They verify mappings/capabilities, inaccessible host files, clean credentials,
file persistence, bounded output, process termination, cancellation, shell timeout,
failed mapping and provisioning, and installing `jq` only in one workspace with
persistence after reopening it. A full service test uses mocked Telegram and
Ollama endpoints with a real sandbox; it checks polling, authorization, tool
continuation, topic replies, and host state. These tests send no live Telegram
messages. Test-created rootfs directories are removed through the mapped worker.
See [VALIDATION.md](VALIDATION.md) for measured results and limitations.
