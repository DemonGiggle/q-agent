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
- **Stay in control.** Check the current setup with `/status`, stop a task with
  `/cancel`, or start a fresh conversation with `/new` without deleting files.

## A few ways to use it

**Turn a folder into a useful summary**

> Look through the notes in this chat's folder and write a short weekly summary.

**Get help with a project**

> Check this repo's changes and explain what I should review first.

**Remember the small things**

> Remind me to pick up my keyboard today at 12:30, Asia/Taipei.

**Give future-you a head start**

> Every Monday at 09:00 Asia/Taipei, inspect this repo and summarize what's changed.

You can also ask to list or delete your schedules. A reminder sends your saved
message; a scheduled task calls the agent when it's time to do the work.
Tasks can use files and services accessible from your host — q-agent doesn't
include a dedicated web browser or stock-price feed.

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
`approved_user_ids` and `approved_chat_ids` under `[telegram]`.
**Both your user ID and the chat ID must be approved.** For a private chat,
they normally match. Keep the bot token out of the config file.

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

```sh
install -D -m 0755 ./bin/q-agent "$HOME/.local/bin/q-agent"
"$HOME/.local/bin/q-agent" service install
```

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

### Telegram commands

| Command | What it does |
| --- | --- |
| `/status` | Show your provider, model, activity, process RSS memory, and current settings — even while busy. |
| `/models` | Browse the selected provider’s models and switch this chat’s model. |
| `/provider <name\|default>` | Select a configured provider for this chat/group; `/provider` lists choices. |
| `/model <id\|default>` | Set this chat/group’s model, or inherit the selected provider’s default. |
| `/think <level\|auto\|default>` | Set thinking, use the provider’s automatic behavior, or inherit configured defaults. |
| `/settings [reset]` | Show effective settings, or clear this chat/group’s overrides. |
| `/help` | Show the command list. |
| `/new` | Start a fresh conversation in this topic. Your files and schedules stay. |
| `/cancel` | Ask the current task to stop. Changes already made stay in place. |
| `/ask <request>` | Give the agent a task. Plain messages work in private chats too. |

In groups, address commands to your bot, for example `/status@your_bot`.

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
