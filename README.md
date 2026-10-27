<div align="center">

# q-agent

### Your own AI agent. Right inside Telegram.

Ask questions, work with files, and schedule tasks — without leaving the chat.

[![CI](https://github.com/DemonGiggle/q-agent/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/DemonGiggle/q-agent/actions/workflows/ci.yml)
[![Go](https://img.shields.io/badge/Go-1.27%2B-00ADD8?logo=go&logoColor=white)](https://go.dev/dl/)
[![Telegram](https://img.shields.io/badge/Telegram-bot-26A5E4?logo=telegram&logoColor=white)](https://core.telegram.org/bots)
[![Linux](https://img.shields.io/badge/Runs_on-Linux-FCC624?logo=linux&logoColor=black)](#quick-start)
[![License: MIT](https://img.shields.io/badge/License-MIT-green.svg)](LICENSE)

[Quick start](#quick-start) · [Features](#features) · [Commands](#commands) · [Docs](#docs)

</div>

---

q-agent is a self-hosted assistant you talk to through Telegram. Connect your
preferred model, choose who can use it, and give it tasks in plain language.
It works with files and commands on your Linux host and brings the result back
to the chat.

## Features

- 💬 **Telegram-native** — private chats, groups (mention or reply), and topics with separate histories sharing one folder.
- 🤖 **Bring your model** — Ollama, OpenAI, OpenCode Go, or ChatGPT subscription. Switch per chat with `/provider`, `/model`, `/think`, `/max_steps`.
- 📁 **Files & shell** — read, write, edit, search, and run commands. Host mode by default, optional Debian sandbox for filesystem isolation.
- 🌐 **Web built in** — DuckDuckGo search and text web fetching, no API key needed.
- 🗓️ **Reminders & schedules** — one-time reminders and daily/weekly agent tasks, just by asking.
- 🧠 **Memory** — short-term conversation context plus opt-in nightly notes per chat.
- 👋 **Companion** — quiet opt-out-able check-ins during your active hours.
- 🖼️ **Photos** — send an image with a caption for vision-capable models.
- 🖥️ **Interactive apps** — drive terminal and background processes, including login flows that reply mid-task.
- 🔄 **Steer & control** — live `Working…` progress, steer running tasks with follow-up text, `/cancel` to stop, `/new` for a fresh start.
- ⌨️ **Console & service** — guided `models` / `config` CLI for humans and scripts, systemd user service, `q-agent update` plus Telegram `/update` and `/restart`.

> Details, limits, and configuration for everything above live in [Docs](#docs).

## Quick start

Needs **Linux**, **Go 1.27+**, **Git**, **Make**, a bot token from [@BotFather](https://t.me/BotFather), and a model provider (example config uses Ollama with `qwen3`).

```sh
git clone https://github.com/DemonGiggle/q-agent.git
cd q-agent
make build
mkdir -p ~/.config/q-agent
cp config.example.toml ~/.config/q-agent/config.toml
```

Edit `~/.config/q-agent/config.toml` to pick your provider/model and set
`allowed_users` / `allowed_groups`, then:

```sh
export TELEGRAM_BOT_TOKEN='your BotFather token'
./bin/q-agent serve
```

Open your bot in Telegram and try `/status` or `/help`. In groups, disable
privacy via BotFather and mention the bot.

Prefer menus over TOML? See the [console guide](docs/console.md).
Keeping it running? See [systemd](docs/systemd.md) and [setup](docs/operations.md#1-setup).

## Commands

| Command | What it does |
| --- | --- |
| `/status`, `/help` | Show setup snapshot · list commands |
| `/models`, `/provider`, `/model`, `/think`, `/max_steps`, `/settings` | View or switch this chat's model setup |
| `/memory`, `/forget`, `/companion` | View memory · clear notes · tune check-ins |
| `/new`, `/cancel` | Fresh conversation · stop the running task |
| `/update`, `/restart` | Admin-only service update/restart |

Full behavior — addressing, photos, steering, schedules, memory, admin setup —
is in [talking to the bot](docs/operations.md#3-talking-to-the-bot).

## Docs

| Guide | Covers |
| --- | --- |
| [Configuration & operations](docs/operations.md) | Providers, credentials, host vs sandbox, tools, schedules, memory, companion, state, upgrades |
| [systemd deployment](docs/systemd.md) | Install, start, sandbox rules, `/restart` & `/update`, removal |
| [Console guide](docs/console.md) | Guided `models` / `config` menus and script-friendly actions |
| [Example configuration](config.example.toml) | All settings in one place |
| [Validation notes](docs/VALIDATION.md) | Test coverage and measured limitations |

A few things worth knowing: tools run with your service account's permissions
(see [host vs sandbox](docs/operations.md#2-how-tools-run-host-vs-sandbox));
schedules tick once per minute; photos are the only supported attachment.
Browser logins arrive as interim messages — reply in the same topic.

---

Built from [qcode](NOTICE), with its MIT attribution retained.
[MIT licensed](LICENSE). Self-hosted. Yours to run.
