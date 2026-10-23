<div align="center">

# q-agent

### Your own AI agent. Right inside Telegram.

Ask questions, work with files, and schedule tasks — without leaving the chat.

[![CI](https://github.com/DemonGiggle/q-agent/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/DemonGiggle/q-agent/actions/workflows/ci.yml)
[![Go](https://img.shields.io/badge/Go-1.27%2B-00ADD8?logo=go&logoColor=white)](https://go.dev/dl/)
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

You'll need **Linux**, **Go 1.27+**, **Git**, **Make**, a Telegram bot token from
[@BotFather](https://t.me/BotFather), and a model provider. The example configuration
uses Ollama with `qwen3`; have Ollama running and that model available, or choose
another provider in the [setup guide](docs/operations.md#1-setup).

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
reply to it, or mention it (for example `<request> @your_bot`). Replies stay in the original topic.
See the [group setup details](docs/operations.md#1-setup).

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
[upgrade guide](docs/operations.md#6-upgrading) for permissions and release details.

For a systemd user service, configured administrators can instead send `/update`
or `/restart` in a private Telegram chat. Both also work for a foreground
`serve` process via a detached helper (SIGTERM + re-exec); without re-exec
argv, `/restart` is rejected and `/update` installs without stopping and asks
for a manual restart. These host-side commands do not call
the model and also work in sandbox mode. See the
[service administration setup](docs/systemd.md#restart-and-update).

### Telegram commands

| Command | What it does |
| --- | --- |
| `/update` | Install the latest stable GitHub Release and restart (systemd or foreground serve with re-exec; otherwise install without stopping); private administrator only. |
| `/restart` | Restart the service (systemd or foreground re-exec); private administrator only. |
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

Send a message to give the agent a task. In groups, mention the bot or reply to it; private chats accept text directly. Address commands to your bot, for example `/status@your_bot`.

**Photo input**

Send a Telegram photo with a question in its caption. In groups, include
`@your_bot` in the caption, or reply to a photo with `@your_bot` and your question.
Private chats also accept photos without captions (the bot describes the image).
Photos received while your task is running become follow-up input at the next safe
boundary. Use a vision-capable model; text-only models may reject image input.
The largest Telegram photo size is downloaded, up to 10 MB per image. Images are
sent to your configured model provider for that task only; binary data and file
IDs are not saved in conversation history. Resend/reply to the image for later
tasks. Albums and images uploaded as documents are not supported.

Full behavior — history clearing, busy rules, steering, schedules, memory —
lives in [talking to the bot](docs/operations.md#3-talking-to-the-bot).
In short: `/provider` / `/model` switches start fresh topic history next task;
failed tasks save bounded context you can continue with a follow-up;
`/status` shows live RSS; new text in the same chat/topic steers the running
task at the next safe boundary.

## A few things worth knowing

- **It runs where you host it.** By default, tools use your Linux service
  account's permissions. Chat folders organize files; they are not security
  boundaries. An optional [Debian sandbox](docs/operations.md#2-how-tools-run-host-vs-sandbox)
  adds filesystem isolation.
- **Schedules are minute-level, not instant.** Busy tasks may wait. Short
  outages have a five-minute catch-up window; interrupted agent work isn't
  automatically retried.
- **Attachment support is limited.** Telegram photos are supported as model
  input; documents, video, audio, and albums are not. Provider charges, if any,
  depend on your chosen service.

Interactive terminal apps, REPLs and temporary servers can use task-scoped
[process sessions](docs/operations.md#interactive-process-sessions). Browser
login instructions can arrive as interim Telegram messages; reply in the same
topic while the task remains active. Sessions end with the task.

## Go deeper

- [Configuration & operations](docs/operations.md) — providers, credentials,
  sandbox setup, scheduling behavior, and backups.
- [Example configuration](config.example.toml) — available settings in one place.
- [Validation notes](docs/VALIDATION.md) — testing coverage and measured limitations.
- [GitHub Actions](https://github.com/DemonGiggle/q-agent/actions/workflows/ci.yml)
  — automated tests, race checks, formatting, vet, and builds.

---

Built from [qcode](NOTICE), with its MIT attribution retained.
[MIT licensed](LICENSE). Self-hosted. Yours to run.
