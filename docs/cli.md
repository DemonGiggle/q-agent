# Command line overview

[← Overview](../README.md) · [Console guide](console.md) · [systemd deployment](systemd.md) · [Configuration & operations](operations.md)

Everything runs through one static binary. `--config FILE` selects the TOML
configuration (default `~/.config/q-agent/config.toml`). `q-agent --help` and
`q-agent <command> --help` describe each command without loading configuration.

| Command | What it is for | Details |
| --- | --- | --- |
| `serve` | Run the bot in the foreground | [Setup](operations.md#1-setup) |
| `models` | Review and change providers/models, guided or scripted | [Console guide](console.md#models) |
| `config` | Show, edit, and validate settings | [Console guide](console.md#other-settings) |
| `login chatgpt` | Connect a ChatGPT subscription via OAuth | See `chatgpt` comments in [example configuration](../config.example.toml) |
| `service install`, `service print` | Install or preview the systemd user unit | [systemd deployment](systemd.md#install) |
| `image build` | Build the Debian base image for sandbox mode | [Host vs sandbox](operations.md#2-how-tools-run-host-vs-sandbox) |
| `update` | Install the latest GitHub Release over the current binary | [Upgrading](operations.md#6-upgrading) |
| `version` | Print the binary version | — |

`models` and `config` open a guided menu on their own and accept explicit
actions (`show`, `get`, `set`, …) for scripts and agents — see the
[interaction contract](console.md#the-interaction-contract). `serve`,
`image build`, and `version` are non-interactive; a config edit never starts or
restarts the service.
