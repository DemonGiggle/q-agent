<div align="center">

# q-agent

### Your own AI agent. Right inside Telegram.

Ask questions, work with files, and schedule tasks — without leaving the chat.

[![CI](https://github.com/DemonGiggle/q-agent/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/DemonGiggle/q-agent/actions/workflows/ci.yml)
[![Go](https://img.shields.io/badge/Go-1.22%2B-00ADD8?logo=go&logoColor=white)](https://go.dev/dl/)
[![Telegram](https://img.shields.io/badge/Telegram-bot-26A5E4?logo=telegram&logoColor=white)](https://core.telegram.org/bots)
[![Linux](https://img.shields.io/badge/Runs_on-Linux-FCC624?logo=linux&logoColor=black)](#quick-start)
[![License: MIT](https://img.shields.io/badge/License-MIT-green.svg)](LICENSE)

[Get started](#quick-start) · [What you can do](#a-few-ways-to-use-it) · [Configuration & operations](docs/operations.md)

</div>

---

## Less switching. More doing.

q-agent is a self-hosted assistant you talk to through Telegram. Connect your
preferred model, choose who can use it, and give it tasks in plain language.
It can work with files and run commands on the machine where you host it,
then bring the result back to your conversation.

- **A familiar place to work.** Use a private chat, mention the bot in a group,
  or keep separate conversations in Telegram topics.
- **Pick up where you left off.** Chat files and conversation history persist
  across restarts. Topics keep separate histories while sharing the chat's files.
- **Make time for later.** Create one-time reminders or daily and weekly tasks
  just by asking. Results return to the chat and topic where you scheduled them.
- **Bring your model.** Use Ollama for a local model, or connect OpenAI or
  OpenCode Go.
- **Look things up.** Search with DuckDuckGo and read web pages, without a
  search API key or extra setup.
- **Stay in control.** Check the current setup with `/status`, stop a task with
  `/cancel`, or start a fresh conversation with `/new` without deleting files.

## A few ways to use it

**Turn a folder into a useful summary**

> Look through the notes in this chat's folder and write a short weekly summary.

**Get help with a project**

> Check this repo's changes and explain what I should review first.

**Find and read something on the web**

> Search DuckDuckGo for Go's latest release, read the official announcement,
> and summarize the changes with source links.

**Remember the small things**

> Remind me to pick up my keyboard today at 12:30, Asia/Taipei.

**Give future-you a head start**

> Every Monday at 09:00 Asia/Taipei, inspect this repo and summarize what's changed.

You can also ask to list or delete your schedules. A reminder sends your saved
message; a scheduled task calls the agent when it's time to do the work.
Tasks can use files and services accessible from your host, DuckDuckGo web search,
and text-only web fetching. Pages requiring JavaScript, logins or bot challenges
aren't supported; q-agent doesn't include an interactive browser or stock-price feed.

## Quick start

You'll need **Linux**, **Go 1.22+**, **Git**, **Make**, a Telegram bot token from
[@BotFather](https://t.me/BotFather), and a model provider. The example configuration
uses Ollama with `qwen3`; have Ollama running and that model available, or choose
another provider in the [setup guide](docs/operations.md#setup).

### 1. Get q-agent

```sh
git clone https://github.com/DemonGiggle/q-agent.git
cd q-agent
make build
mkdir -p ~/.config/q-agent
cp config.example.toml ~/.config/q-agent/config.toml
```

### 2. Make it yours

Edit `~/.config/q-agent/config.toml` to choose your provider and model, then set
`allowed_users` and `allowed_groups` under `[telegram]`.
**Private chats list your user ID once in `allowed_users`** (that ID authorizes
that private chat). **Groups list the group ID in `allowed_groups`** — anyone in
an allowed group can talk, with no per-user listing. Keep the bot token out of
the config file.

Prefer a guided setup? Run `./bin/q-agent models` to review and change your
model, or `./bin/q-agent config` to edit other settings. Each change is previewed
before you save. [Console guide →](docs/console.md)

### 3. Start chatting

```sh
export TELEGRAM_BOT_TOKEN='your BotFather token'
./bin/q-agent serve
```

Runtime and configuration commands use `~/.config/q-agent/config.toml` by default. Use `--config FILE`
to select another configuration file.

Open your bot's private chat and send a message. Try `/status` to see your setup
or `/help` to see the available commands.

**Using a group?** Disable bot privacy through BotFather, then mention the bot,
reply to it, or send `/ask@your_bot <request>`. Replies stay in the original topic.
See the [group setup details](docs/operations.md#setup).

### Keep it running with systemd

Use **`~/.local/bin/q-agent`** as the permanent executable location. From a source
checkout:

```sh
install -D -m 0755 ./bin/q-agent "$HOME/.local/bin/q-agent"
"$HOME/.local/bin/q-agent" service install
```

For an extracted release archive, use `./q-agent` instead of `./bin/q-agent`.
Run the installer as your normal service account, without `sudo`.

This installs a **user service** and creates a private config template only if
missing. Review the config, allowlists and credential files, then enable it with
`systemctl --user enable --now q-agent.service` after `systemctl --user daemon-reload`.
Existing configuration is preserved; the installer never starts the bot.
Host and sandbox modes are supported. See the [systemd guide](docs/systemd.md)
for exact file locations, boot-without-login setup, credentials and sandbox requirements.

## Your everyday controls

### Upgrade q-agent

```sh
q-agent version
q-agent update
```

The update command installs the latest GitHub Release for Linux amd64 or arm64
over the executable you invoked. If you run from the source checkout, use
`./bin/q-agent update`. It preserves configuration and chat data; restart your
running service when ready to use the new version. See the
[upgrade guide](docs/operations.md#upgrading) for permissions and release details.

For a systemd user service, configured administrators can instead send `/update`
or `/restart` in a private Telegram chat. These host-side commands do not call
the model and also work in sandbox mode. See the
[service administration setup](docs/systemd.md#telegram-administration).

### Telegram commands

| Command | What it does |
| --- | --- |
| `/update` | Install the latest stable GitHub Release and restart; private administrator only. |
| `/restart` | Restart the systemd user service; private administrator only. |
| `/status` | Show your provider, model, activity, process RSS memory, and current settings — even while busy. |
| `/models` | Browse the selected provider’s models and switch this chat’s model. |
| `/provider <name\|default>` | Select a configured provider for this chat/group; `/provider` lists choices. |
| `/model <id\|default>` | Set this chat/group’s model, or inherit the selected provider’s default. |
| `/think <level\|auto\|default>` | Set thinking, use the provider’s automatic behavior, or inherit configured defaults. |
| `/max_steps [number\|default]` | Show or set this chat/group’s maximum model steps; `default` inherits the global setting. |
| `/settings [reset]` | Show effective settings, or clear this chat/group’s overrides. |
| `/memory` | Show notes, pending sources, retry state, and loss counters for this chat. |
| `/forget` | Clear notes and pending memory sources across all topics. Files and history stay. |
| `/help` | Show the command list. |
| `/new` | Start a fresh conversation in this topic. Your files and schedules stay. |
| `/cancel` | Ask the current task to stop. Changes already made stay in place. |
| `/ask <request>` | Give the agent a task. Plain messages work in private chats too. |

In groups, address commands to your bot, for example `/status@your_bot`.

**Per-chat model-step limit**

Use `/max_steps` to view the effective maximum number of model requests per
agent task, `/max_steps 20` to set it to a positive integer, and
`/max_steps default` to inherit `[limits].max_steps` from the configuration.
The setting persists across restarts and `/new`, and is shared by all topics
in a chat/group, including scheduled agent tasks. Changes are rejected while
the chat has an active task. `/settings reset` clears this override too.
Changing the limit does not clear conversation history or change the separate
tool-call limit or task timeout. In groups, address the command to the bot,
for example `/max_steps@your_bot 20`.

When a running task hits a step/tool limit, provider error, or timeout, its
original request, applied follow-ups, failure reason, and bounded tool observations
are saved in this topic's short-term history. Adjust the limit (for example
`/max_steps 32`) and send a follow-up such as "continue" in the same topic.
Completed tool calls are not automatically replayed. Records persist across
restart but follow the ordinary history retention limits; `/new` clears them.
Explicit cancellation does not add the cancelled run to history. This is saved
context, not an exact execution checkpoint or recovery from a process crash.

Successful interactive and scheduled tasks are captured in a durable memory
journal before prompt history is bounded. Startup and nightly digests process
that journal across all topics. `/new`, model/provider changes, and execution
mode changes retain pending sources. `/forget` clears notes and pending sources
for the entire chat; a running task may add a new source when it finishes.

Memory uses hard UTF-8 byte limits. Pending sources default to 4 MiB; overflow
removes the oldest sources with a warning and loss counters. Exceptional events
retain a marked head and tail within 256 KiB. Old summaries survive the one-time
storage migration within the configured budget; pre-upgrade pending history is
outside the new baseline and is not imported. See [memory operations](docs/operations.md#long-term-memory).

`/status` reports the q-agent bot process's current resident memory (RSS) in MiB,
read from Linux `/proc/self/status` on each request. This is physical resident
memory, not virtual size, Go heap usage, or a historical peak. It includes shared
resident pages and excludes sandbox workers and other child processes. If the
kernel metric cannot be read, it shows `Unavailable` instead of a substitute.

**Want to adjust a task while it runs?** Send another message in the same
chat/topic. Your follow-up steers your current task at the next safe boundary,
without cancelling an operation already in progress. Only the task's original
sender can steer it; other topics and users do not change its instructions.
Follow-ups still waiting to be consumed are merged in order and sent together.
Control commands such as `/status` and `/cancel` keep their usual behavior.

## A few things worth knowing

- **It runs where you host it.** By default, tools use your Linux service
  account's permissions. Chat folders organize files; they are not security
  boundaries. An optional [Debian sandbox](docs/operations.md#sandbox-opt-in)
  adds filesystem isolation.
- **Schedules are minute-level, not instant.** Busy tasks may wait. Short
  outages have a five-minute catch-up window; interrupted agent work isn't
  automatically retried.
- **The experience is text-first.** Attachments and streaming Telegram replies
  aren't supported yet. Provider charges, if any, depend on your chosen service.

## Go deeper

- [Configuration & operations](docs/operations.md) — providers, credentials,
  sandbox setup, scheduling behavior, and backups.
- [Example configuration](config.example.toml) — available settings in one place.
- [Validation notes](VALIDATION.md) — testing coverage and measured limitations.
- [GitHub Actions](https://github.com/DemonGiggle/q-agent/actions/workflows/ci.yml)
  — automated tests, race checks, formatting, vet, and builds.

---

Built from [qcode](NOTICE), with its MIT attribution retained.
[MIT licensed](LICENSE). Self-hosted. Yours to run.
