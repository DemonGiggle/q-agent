<div align="center">

# q-agent

### Your own AI agent. Right inside Telegram.

Ask questions, work with files, and schedule tasks — without leaving the chat.

[![CI](https://github.com/DemonGiggle/q-agent/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/DemonGiggle/q-agent/actions/workflows/ci.yml)
[![Go](https://img.shields.io/badge/Go-1.27%2B-00ADD8?logo=go&logoColor=white)](https://go.dev/dl/)
[![Telegram](https://img.shields.io/badge/Telegram-bot-26A5E4?logo=telegram&logoColor=white)](https://core.telegram.org/bots)
[![Linux](https://img.shields.io/badge/Runs_on-Linux-FCC624?logo=linux&logoColor=black)](#quick-start)
[![License: MIT](https://img.shields.io/badge/License-MIT-green.svg)](LICENSE)

[Quick start](#quick-start) · [Highlights](#highlights) · [Commands](#commands) · [Docs](#docs)

</div>

---

q-agent is a self-hosted assistant you talk to through Telegram. Connect your
preferred model, choose who can use it, and give it tasks in plain language.
It works with files and commands on your Linux host and brings the result back
to the chat.

## Highlights

### 🪶 Tiny footprint

Single static Go binary (~9 MB), no runtime dependencies. Idles under ~20 MB
RSS — verify anytime with `/status`. Runs fine on a small VPS or Raspberry Pi,
as a systemd user service. See [systemd deployment](docs/systemd.md).

### 📦 Native sandbox, no Docker

Opt-in filesystem isolation with bubblewrap (needs `bubblewrap` installed,
plus `uidmap` / `mmdebstrap` on Debian to build the image). Each chat gets its
own Debian rootfs; tools run with a clean env, credentials never enter the
sandbox, and failures never silently fall back to host mode. Host mode stays
the default for speed. See [host vs sandbox](docs/operations.md#2-how-tools-run-host-vs-sandbox).

### 👋 Companion mode, on by default

q-agent checks in quietly instead of waiting to be asked:

- **How it works:** a sweep wakes at random moments inside `active_time`
  (default `09:00-22:00`), a few times per day. It reads addressed history +
  memory notes + live group chatter (`recent_window`, default 12h), then makes
  one cheap no-tools model call that decides `silent` / `reminder` / `warmth` /
  `comfort` / `suggestion`. Silence is a healthy outcome.
- **Throttled:** at most 1 message per chat per sweep, max 1/day with a 6h
  cooldown (rolling 24h, survives restarts). No tools, no shell, no web, no
  cross-chat profiling.
- **Safe:** skips unauthorized chats, busy tasks, recent activity (default 60m),
  and empty input. Groups default to reminders-only.
- **Yours to tune:** `/companion` shows buttons plus today's sweep diagnostics;
  `/companion on|off|default|status|history` per chat, `[proactive]
  enabled=false` as the global kill switch.

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
