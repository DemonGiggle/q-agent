<div align="center">

# q-agent

### Your own AI agent. Right inside Telegram.

Ask questions, work with files, and schedule tasks — without leaving the chat.

[![CI](https://github.com/DemonGiggle/q-agent/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/DemonGiggle/q-agent/actions/workflows/ci.yml)
[![Go](https://img.shields.io/badge/Go-1.27%2B-00ADD8?logo=go&logoColor=white)](https://go.dev/dl/)
[![Telegram](https://img.shields.io/badge/Telegram-bot-26A5E4?logo=telegram&logoColor=white)](https://core.telegram.org/bots)
[![Linux](https://img.shields.io/badge/Runs_on-Linux-FCC624?logo=linux&logoColor=black)](#quick-start)
[![License: MIT](https://img.shields.io/badge/License-MIT-green.svg)](LICENSE)

[Quick start](#quick-start) · [Highlights](#highlights) · [Telegram](#telegram-commands) · [Command line](#command-line) · [Docs](#docs)

</div>

---

q-agent is a self-hosted assistant you talk to through Telegram. Connect your
preferred model, choose who can use it, and give it tasks in plain language.
It works with files and commands on your Linux host and brings the result back
to the chat.

## Highlights

### 🪶 Tiny footprint

All the agent, none of the bloat:

- **Featherweight:** one static binary that idles under ~20 MB — confirm with
  `/status` anytime.
- **Runs anywhere:** happy on a small VPS or Raspberry Pi, with no runtime
  dependencies to babysit.
- **Stays out of your way:** installs as a systemd user service and just runs.
  See [systemd deployment](docs/systemd.md).

### 📦 Native sandbox, no Docker

Let it do real work — without handing over the keys:

- **Truly isolated:** each chat gets its own Debian world, powered by
  bubblewrap (just needs `bubblewrap` installed; on Debian, `uidmap` /
  `mmdebstrap` build the image).
- **Credentials stay out:** your keys live on the host and never enter the
  sandbox.
- **Honest by design:** if isolation can't be guaranteed, it stops instead of
  silently running unprotected. Fast host mode stays the default for speed.
  See [host vs sandbox](docs/operations.md#2-how-tools-run-host-vs-sandbox).

### 👋 Companion mode, on by default

Your agent doesn't just wait to be asked — it looks out for you:

- **It reaches out first:** a gentle reminder, a word of encouragement, or a
  timely suggestion, grounded in your recent conversations and memory.
- **Quiet by nature:** it knows silence is usually the right answer, and never
  interrupts when you're busy or recently active.
- **On your terms:** tune it per chat from `/companion`, or turn it off
  entirely.

Full rules in [operations](docs/operations.md#3-talking-to-the-bot).

## More features

- Telegram: private chats, groups (mention or reply), topics with separate histories.
- Models: Ollama, OpenAI, OpenCode Go, ChatGPT subscription; per-chat `/provider`, `/model`, `/think`, `/max_steps`.
- Tools: `read`, `list`, `search`, `write`, `edit`, `shell` with capped output.
- Web: `web_search` (DuckDuckGo, no key), `web_fetch` (text-only, no JS/login).
- Schedules: one-time reminders and daily/weekly agent tasks by asking; list/delete in chat.
- Memory: short-term context per topic plus nightly notes per chat (`/memory`, `/forget`).
- Photos: image + caption input for vision-capable models.
- Interactive sessions: PTY/pipe process sessions (`shell` + `process`), incl. login flows.
- Control: live `Working…` progress, steer running tasks with follow-up text, `/cancel`, `/new`.
- Console: guided `models` / `config` menus plus script-friendly `--json` actions.
- Service: systemd user unit, `q-agent update`, Telegram `/update` + `/restart` (admin only).

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

## Telegram commands

| Command | What it does |
| --- | --- |
| `/status`, `/help` | Show setup snapshot · list commands |
| `/models`, `/provider`, `/model`, `/think`, `/max_steps`, `/settings` | View or switch this chat's model setup |
| `/memory`, `/forget`, `/companion` | View memory · clear notes · tune check-ins |
| `/new`, `/cancel` | Fresh conversation · stop the running task |
| `/update`, `/restart` | Admin-only service update/restart |

Full behavior — addressing, photos, steering, schedules, memory, admin setup —
is in [talking to the bot](docs/operations.md#3-talking-to-the-bot).

## Command line

The same binary also runs the bot, edits settings, manages the service, and
self-updates — all from your shell.

See the [command line overview](docs/cli.md) for what each command is for.

## Docs

| Guide | Covers |
| --- | --- |
| [Command line overview](docs/cli.md) | Every `q-agent` command and where its details live |
| [Configuration & operations](docs/operations.md) | Providers, credentials, host vs sandbox, tools, schedules, memory, companion, state, upgrades |
| [systemd deployment](docs/systemd.md) | Install, start, sandbox rules, `/restart` & `/update`, removal |
| [Console guide](docs/console.md) | Guided `models` / `config` menus and script-friendly actions |
| [Example configuration](config.example.toml) | All settings in one place |
| [Validation notes](docs/VALIDATION.md) | Test coverage and measured limitations |

---

Built from [qcode](NOTICE), with its MIT attribution retained.
[MIT licensed](LICENSE). Self-hosted. Yours to run.
