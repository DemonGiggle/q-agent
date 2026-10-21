# Running q-agent with systemd

[← Overview](../README.md) · [Configuration & operations](operations.md)

A **systemd user service**, running as the non-root Linux account that owns
its config and state. Host mode and sandbox mode use the same unit. There is
no separate daemon or root service.

Concepts (execution modes, file locations, credentials) are explained in
[operations §2](operations.md#2-how-tools-run-host-vs-sandbox). This guide
covers only what is specific to running under systemd.

## Install

Recommended location: **`~/.local/bin/q-agent`** — outside checkouts and
download folders, at a permanent path owned by the service account.

Run as the intended service account, **without sudo**. Example for v0.1.0:

```sh
tar -xzf q-agent_v0.1.0_linux_amd64.tar.gz
install -D -m 0755 ./q-agent "$HOME/.local/bin/q-agent"
"$HOME/.local/bin/q-agent" service install
```

From a source checkout instead:

```sh
make build
install -D -m 0755 ./bin/q-agent "$HOME/.local/bin/q-agent"
"$HOME/.local/bin/q-agent" service install
```

`service install` creates the user unit and, only if missing, a private TOML
template. It never enables/starts the bot, reloads systemd, imports secrets,
or overwrites existing settings. A different existing unit (even a symlink)
is refused, never replaced. Preview without writing:

```sh
"$HOME/.local/bin/q-agent" service print
"$HOME/.local/bin/q-agent" service install --config /absolute/path/config.toml
```

The unit records the absolute binary path. Keep that binary in place
(no `go run`, no temp paths). Paths with quotes/backslashes are rejected.

### Files

| Item | Default | Notes |
| --- | --- | --- |
| Executable | `~/.local/bin/q-agent` | Service account, executable |
| Settings | `~/.config/q-agent/config.toml` | New template is `0600` |
| Telegram credential | `~/.config/q-agent/telegram.token` | Raw token only, `0600`/`0400`, no symlink |
| Provider credential | `~/.config/q-agent/provider.key` | Same strict rules |
| Optional env file | `service.env` next to config | `0600` recommended, not a shell script |
| Unit | `~/.config/systemd/user/q-agent.service` | New file is `0644` |
| State, chat folders | `~/.local/share/q-agent/` | Created privately at runtime |
| Logs | user's journal | `journalctl --user -u q-agent.service` |

An absolute `XDG_CONFIG_HOME` moves only the **unit** path. `--config`
selects a different app config. Relative TOML paths resolve against the
config's directory; `~` is the service account's home. The unit's working
directory is that home.

### Credentials

Generate the config template, then edit provider/model and set
`allowed_users` / `allowed_groups` (empty by default = startup refuses).
The template uses `token_path = "telegram.token"`: create that file with
**only the raw bot token** and `chmod 0600` it. Provider keys follow the
same rules; never set `*_env` and `*_path` at the same time.

If you keep env-based config, put assignments in `service.env`:

```text
TELEGRAM_BOT_TOKEN=[REDACTED]
OPENAI_API_KEY=sk-...
```

No `export`, no substitution, no `~`/`$PATH` expansion. Your interactive
shell's exports are not inherited by the user service. Nothing reads or
copies credential contents at install time.

## Start and operate

```sh
"$HOME/.local/bin/q-agent" config validate
systemctl --user daemon-reload
systemctl --user enable --now q-agent.service
systemctl --user status q-agent.service
journalctl --user -u q-agent.service -n 100 --no-pager
```

`config validate` checks TOML/settings only — not credentials, allowlists,
or sandbox readiness. Startup enforces those and logs actionable errors.

The unit restarts on failure after 5s; explicit stops stay stopped.
A stop sends SIGTERM with up to 30s to shut down. `UMask=0077` keeps new
files private.

```sh
systemctl --user stop q-agent.service
systemctl --user start q-agent.service
systemctl --user restart q-agent.service
journalctl --user -u q-agent.service -f
```

For boot without login, an administrator must enable lingering for the
**same** account (the installer never does this):

```sh
sudo loginctl enable-linger "$USER"
loginctl show-user "$USER" -p Linger
```

If `systemctl --user` cannot connect, establish a normal login or ask for
lingering. Never `sudo q-agent service install` (wrong home/ownership).
No system unit is installed; for a dedicated identity, create that account
first and run these steps as it. Keep UID/GID and subordinate ranges stable.

## Sandbox on systemd

Set `sandbox.enabled = true`, then as the same non-root account:

```sh
systemctl --user stop q-agent.service
"$HOME/.local/bin/q-agent" config set sandbox.enabled true
"$HOME/.local/bin/q-agent" image build
systemctl --user start q-agent.service
```

Prerequisites are the same as foreground mode
([operations §2](operations.md#2-how-tools-run-host-vs-sandbox)): namespaces,
`bubblewrap uidmap mmdebstrap debian-archive-keyring`, ≥ 65,536 IDs in both
`/etc/subuid` and `/etc/subgid`, static binary, built image. Additionally:

- The unit must **not** set `NoNewPrivileges=yes`, `RestrictNamespaces`,
  `PrivateUsers`, `CapabilityBoundingSet`, or syscall filters that block
  unprivileged namespaces and `newuidmap`/`newgidmap`. Do not add them in
  drop-ins.
- Do not change service identities or subordinate allocations after chat
  rootfs trees are provisioned.
- Switching modes clears conversation history (files stay in their own
  trees). Missing image/helpers/allocations fail startup — no host fallback.

## Restart and update

`/restart` and `/update` are handled directly by the host — not the model,
not the shell tool — in both host and sandbox modes. Under systemd the
process must be the actual `MainPID` of the same account's **user**
`q-agent.service`. (Foreground `serve` uses a detached `Setsid` helper
instead: `service maintain --state-dir ... --id ...` SIGTERMs the old
process and re-execs `serve`. System units and differently named units are
rejected. Needs `systemctl`, `systemd-run`, a working user manager, and
write access to the executable's directory. No sudo.)

Disabled by default. Enable for your own private chat:

```toml
[telegram]
allowed_users = [123456789]
admin_user_ids = [123456789]   # must also be in allowed_users
```

```sh
q-agent config set telegram.admin_user_ids '[123456789]'
```

Restart once through systemd after enabling, then send the command with no
arguments in the administrator's **private** chat. Groups/topics are always
rejected. Neither command calls the model or provisions a workspace.

- Refused while any interactive or scheduled task is active. While
  maintenance is pending, new work is refused; scheduled work stays queued.
- A helper launches before stopping: transient
  `q-agent-maintenance-<id>.service` via `systemd-run --user` (separate
  cgroup, so stopping the bot cannot kill it), or a detached process for
  foreground serve (logs at `state_dir/maintenance/helper-<id>.log`, new
  output at `state_dir/maintenance/serve.log`). The bot arms the request
  only after Telegram confirms the pre-stop notice.
- `/restart`: stop + start (or SIGTERM + re-exec foreground), then wait up
  to 60s for a new PID and the readiness marker (startup checks, Telegram
  identity, store/scheduler init — not a model completion proof).
- `/update`: same, but first installs the latest **stable GitHub Release**
  over the executable (never builds, never pulls `main`, no arch/URL flags,
  static ELF validated). A same-directory backup is kept; download or
  readiness failure restores it and restarts. A custom `sandbox.worker`
  pointing elsewhere refuses `/update` (`/restart` still works).
- Results queue through the normal redacted outbox. Duplicates and
  concurrent requests cannot run it twice. Config, credentials, chat files,
  and base images are untouched. The helper has a 10-minute limit plus a
  start-on-exit fallback; state lives in
  `state_dir/maintenance/request.json`. Rollback does not reverse future
  state/config migrations.

Debug without editing state while the helper runs:

```sh
journalctl --user -u q-agent.service -u 'q-agent-maintenance-*.service' -n 100 --no-pager
systemctl --user list-units --all 'q-agent-maintenance-*'
```

If recovery cannot start the bot, no Telegram result can be delivered —
restore it from the journal and the saved backup.

Both commands require a binary that already knows them; install the feature
manually first. The target Release must speak the readiness protocol
(e.g. `v0.1.0` predates it and rolls back).

## Upgrade or remove the unit

Manual upgrade (archive release): wait for tasks, stop, replace, restart.
Keep existing config/state; ignore the archive's example config. (v0.1.0's
updater expects raw binaries — use this manual path for `.tar.gz`. Newer
updaters accept either, extracting only the top-level executable. Once
enabled above, `/update` automates stop → install → readiness → recovery.)

```sh
systemctl --user stop q-agent.service
install -D -m 0755 ./q-agent "$HOME/.local/bin/q-agent"
systemctl --user start q-agent.service
```

Same-path binary/config changes need only a restart; unit-file changes need
`daemon-reload` too. Moving paths: review `service print`, stop, move the
old unit aside, reinstall. The installer never overwrites a customized unit.

```sh
systemctl --user disable --now q-agent.service   # non-destructive
```

Removing the unit never deletes config, credentials, state, chat files, or
images — delete those explicitly if wanted.

## Validation

Unit tests cover install safety, config preservation, quoting, and
`systemd-analyze --user verify` when available. Opt-in probes use uniquely
named **temporary runtime units** and remove them afterwards — they never
touch `q-agent.service` or contact Telegram/providers:

```sh
make build
Q_AGENT_SYSTEMD_INTEGRATION=1 Q_AGENT_WORKER="$PWD/bin/q-agent" \
  go test -v ./internal/service -run TestIntegrationSystemdHostAndSandbox -timeout 2m
Q_AGENT_MAINTENANCE_SYSTEMD_TEST=1 go test -count=1 -v ./internal/maintenance \
  -run TestSystemdMaintenanceSurvivesBotCgroupStop -timeout 1m
Q_AGENT_LIFECYCLE_SYSTEMD_TEST=1 go test -count=1 -v ./cmd/q-agent \
  -run '^TestIntegrationMaintenanceLifecycle$' -timeout 5m
```

The first checks offline host + real rootless sandbox read/write, credential
lookup, `service.env`, and special-character paths (minimal tar, no Debian
image download). The second restarts a disposable probe via the production
controller. The third runs restart/upgrade/rollback in both modes with
loopback Telegram/model/Release fixtures, checking workspace persistence,
version, binary bytes, and preserved config — then cleans up. It proves no
live Telegram or hosted-GitHub upgrade. Ordinary `go test ./...` skips all
three, so CI success alone is not evidence they ran.
