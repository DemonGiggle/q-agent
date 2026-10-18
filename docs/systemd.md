# Running q-agent with systemd

[← Overview](../README.md) · [Configuration & operations](operations.md)

q-agent supports a **systemd user service**, running as the non-root Linux
account that owns its configuration and state. Host mode and the opt-in rootless
sandbox use the same unit. There is no separate daemon process or root service.

## Install and configure

The recommended executable location is **`~/.local/bin/q-agent`**. It keeps the
binary outside your source checkout or Downloads folder, at a permanent path
owned by the service account. Configuration and state stay separately in
`~/.config/q-agent/` and `~/.local/share/q-agent/`.

Run the following commands **as the intended service account, without sudo**.
For a downloaded release, extract the archive matching your architecture, then
install the executable. This example uses v0.1.0 for x86_64:

```sh
tar -xzf q-agent_v0.1.0_linux_amd64.tar.gz
install -D -m 0755 ./q-agent "$HOME/.local/bin/q-agent"
"$HOME/.local/bin/q-agent" service install
```

For aarch64, use `q-agent_v0.1.0_linux_arm64.tar.gz` instead. The binary is named
`q-agent` inside either archive. If building from a source checkout, use:

```sh
make build
install -D -m 0755 ./bin/q-agent "$HOME/.local/bin/q-agent"
"$HOME/.local/bin/q-agent" service install
```

`service install` creates a user unit and, only if missing, a private TOML
configuration template. **It does not enable/start the service, reload systemd,
import secrets from your shell, change subordinate IDs, or install dependencies.**
It preserves every existing config byte and its permissions. A different existing
unit, including a symlink, is rejected rather than replaced. An identical unit
can be installed again without changes.

The unit records the absolute executable path used to invoke the installer.
Keep that binary in place; don't install from `go run` or a temporary build path.
Spaces, dollar signs and percent signs are handled, but systemd forbids executable
paths containing quotes or backslashes. Review without writing anything:

```sh
"$HOME/.local/bin/q-agent" service print
# Or select an existing config; paths are made absolute in the unit:
"$HOME/.local/bin/q-agent" service print --config /absolute/path/config.toml
"$HOME/.local/bin/q-agent" service install --config /absolute/path/config.toml
```

### File locations

| Item | Default location | Ownership / mode |
| --- | --- | --- |
| Executable (recommended) | `~/.local/bin/q-agent` | service account, executable |
| Settings | `~/.config/q-agent/config.toml` | service account; new template is `0600` |
| Telegram credential | `~/.config/q-agent/telegram.token` | service account, `0600` or `0400` |
| Provider credential (optional) | `~/.config/q-agent/provider.key` | service account, `0600` or `0400` |
| Optional environment file | `service.env` next to the selected config | service account, `0600` recommended |
| Unit | `~/.config/systemd/user/q-agent.service` | service account; new file is `0644` |
| State and chat folders | `~/.local/share/q-agent/` | service account; created privately at runtime |
| Logs | user's journal | `journalctl --user -u q-agent.service` |

An absolute `XDG_CONFIG_HOME`, if set, changes the **unit** location to
`$XDG_CONFIG_HOME/systemd/user/q-agent.service`; it does not change q-agent's
existing default config path. Use `--config` for a different app config.
Relative paths inside TOML resolve against that config's directory, not the
service's working directory. `~` refers to the service account's home.
The unit's working directory is the account's home, independent of the checkout.

### Required settings and credentials

Before starting, edit the generated config: choose a provider/model and set
**both** `telegram.approved_user_ids` and `telegram.approved_chat_ids`. The new
template leaves them empty, so startup fails closed until you configure them.
For private chats, the user ID and chat ID normally match.

The generated template uses:

```toml
[telegram]
token_path = "telegram.token"
approved_user_ids = [123456789]  # replace with your Telegram user ID
approved_chat_ids = [123456789] # replace with your chat ID(s)
```

Create `telegram.token` with your editor containing **only the raw bot token**,
not a shell assignment or TOML string. It must be a regular, non-symlink file
owned by the service account:

```sh
chmod 0600 "$HOME/.config/q-agent/telegram.token"
```

For OpenAI or OpenCode Go, set the provider/model, remove the Ollama `base_url`,
and use `api_key_path = "provider.key"` with the same raw-file ownership/mode
rules. Do not simultaneously set a nonempty `api_key_env` and `api_key_path`,
or a nonempty `token_env` and `token_path`.

If preserving an existing environment-based config, place the credential
assignments in `service.env` next to that config, for example
`TELEGRAM_BOT_TOKEN=...` and the configured provider variable. Protect it with
`chmod 0600`. It is a systemd environment file, **not a shell script**: no `export`,
command substitution, `~`, or `$PATH` expansion. Your interactive shell's exported
credentials are not automatically inherited by the user service.

The unit supplies a stable system PATH plus `~/.local/bin`. Add a literal absolute
PATH to `service.env` if your host tools need other executables. Nothing reads or
copies credential contents during installation; serving resolves the credentials
and enforces their file rules.

## Enable, start and operate

After configuration and credentials are ready:

```sh
"$HOME/.local/bin/q-agent" config validate
systemctl --user daemon-reload
systemctl --user enable --now q-agent.service
systemctl --user status q-agent.service
journalctl --user -u q-agent.service -n 100 --no-pager
```

`config validate` checks TOML/settings; it does not authenticate credentials,
check allowlists for completeness, or prove sandbox readiness. Startup enforces
those prerequisites and logs actionable errors.

The unit restarts on failure after five seconds. Explicit stops stay stopped.
Output goes to the journal through q-agent's normal diagnostic redaction.
A stop sends SIGTERM and allows up to 30 seconds for shutdown; systemd's control
group cleanup covers remaining service processes. `UMask=0077` keeps new files
private by default.

```sh
systemctl --user stop q-agent.service
systemctl --user start q-agent.service
systemctl --user restart q-agent.service
journalctl --user -u q-agent.service -f
```

For boot without an interactive login, an administrator must enable lingering
for the **same** account (this is not done by the installer):

```sh
sudo loginctl enable-linger "$USER"
loginctl show-user "$USER" -p Linger
```

A running user manager/session bus is needed for `systemctl --user`. If it cannot
connect, establish a normal login for that account or ask an administrator to
configure lingering; do not run `sudo q-agent service install`, which would
select root's home and the wrong ownership.

No `/etc/systemd/system` unit is installed. To use a dedicated service identity,
set up that non-root account first and run these same user-service steps as it.
Keep its UID, GID and subordinate allocations stable across restarts/restores.

## Sandbox support

**Yes.** Set `sandbox.enabled = true` in the same TOML. The unit intentionally
does **not** set `NoNewPrivileges=yes`, `RestrictNamespaces`, `PrivateUsers`,
`CapabilityBoundingSet`, or syscall filters that prevent unprivileged namespaces
and setuid `newuidmap`/`newgidmap`. Do not add such restrictions in drop-ins if
you expect the current sandbox implementation to work.

Sandbox prerequisites remain the same as foreground mode:

- A non-root service account and kernel support for the required unprivileged
  user/mount/PID/IPC/UTS/cgroup namespaces, permitted by the host's policy.
- On Debian: `bubblewrap`, `uidmap`, `mmdebstrap`, `debian-archive-keyring`.
- At least 65,536 subordinate IDs allocated to that account in **both**
  `/etc/subuid` and `/etc/subgid`. Have an administrator inspect existing entries
  and allocate unused ranges; do not blindly overwrite them or reuse another
  account's range.
- A static q-agent worker (`make build` and release binaries are static) and a
  completed native-architecture Debian base image owned by that account.

Stop the service, then prepare the image as that same non-root account:

```sh
systemctl --user stop q-agent.service
"$HOME/.local/bin/q-agent" config set sandbox.enabled true
"$HOME/.local/bin/q-agent" image build
systemctl --user start q-agent.service
```

Use the same `--config FILE` on config/image commands for a custom installation.
By default the image is `~/.local/share/q-agent/images/debian-trixie.tar` and its
manifest sits alongside it. The worker defaults to the current binary; setting
`sandbox.worker` explicitly is optional. Do not change service identities or
subordinate allocations after provisioning chat rootfs trees.

Changing host/sandbox execution mode resets conversation history and provider
identity; existing files remain in their original host/sandbox trees. Back up
state first. Missing images, mapping helpers, allocations or namespace support
cause sandbox startup/execution to fail; there is **no fallback to host mode**.

Host mode has the account's ordinary host permissions. The systemd unit is not
itself the filesystem isolation boundary. Sandbox mode mounts only the chat
rootfs and trusted worker while host-side config, credentials and state remain
outside; networking deliberately remains available through the host network.

## Telegram administration

`/restart` and `/update` are handled directly by the host, not the model or its
shell tool, in both host and sandbox modes. They require the running process to
be the actual `MainPID` of the same account's **user** `q-agent.service`.
Foreground processes, system-wide units and differently named units are rejected.
The account needs `systemctl`, `systemd-run`, a working user manager, and write
access to the installed executable's directory for updates. No sudo is used.

Administration is disabled by default. Add the operator to all three lists:

```toml
[telegram]
approved_user_ids = [123456789]
approved_chat_ids = [123456789]
admin_user_ids = [123456789]
```

Keep existing entries and credential settings. Administrator IDs must be positive
and also occur in `approved_user_ids`; the private chat must be approved too.
You can edit an existing configuration without discarding comments:

```sh
q-agent config set telegram.admin_user_ids '[123456789]'
```

Restart once through systemd after first installing this feature or changing its
configuration. Afterwards send either command, with no arguments, in the
administrator's **private** chat. Group/topic administration is rejected, even
for an administrator. Neither command invokes the model or provisions a workspace.

- If any interactive or scheduled agent task is active, the command is refused;
  wait for completion or cancel the tasks first. While maintenance is pending,
  new agent work is refused and scheduled work remains durably queued.
- An independent transient `q-agent-maintenance-<id>.service` is launched with
  `systemd-run --user`. It belongs to a separate cgroup, so stopping the bot
  cannot kill its updater. The bot arms the request only after Telegram confirms
  delivery of the pre-stop notice; notification failure cancels the operation.
- `/restart` stops and starts the service, then waits up to 60 seconds for a
  new MainPID and q-agent's readiness marker. Readiness means normal startup
  checks, Telegram identity lookup, store and scheduler initialization succeeded;
  it does not prove a model completion or sandbox tool execution.
- `/update` preserves a same-directory executable backup, stops the service,
  installs the latest **stable GitHub Release**, and starts it. It never builds
  or pulls `main`, accepts no architecture/URL override, and validates the native
  static ELF before replacement. Download or readiness failure restores the
  previous executable and attempts startup again. A separately configured
  `sandbox.worker` must resolve to the service executable, otherwise `/update`
  is refused; `/restart` is still available.
- The restarted bot queues the result and running/previous versions through its
  normal persistent, redacted reply outbox. Duplicate Telegram updates and
  concurrent maintenance requests cannot launch the operation twice. No
  configuration, credentials, chat files or base images are replaced.

This feature must first be installed manually; an older binary does not recognize
the commands or the new `admin_user_ids` setting. The selected Release must also
contain the readiness protocol: an older Release such as `v0.1.0` fails readiness
and is rolled back. Source `dev` builds are not ordered against stable tags, so
they can attempt that older release. Equal/newer semantic versions are not
downgraded, though the requested maintenance still restarts the service.

Inspect failures without editing state while the helper is running:

```sh
journalctl --user -u q-agent.service -u 'q-agent-maintenance-*.service' -n 100 --no-pager
systemctl --user list-units --all 'q-agent-maintenance-*'
```

The helper has a ten-minute lifetime limit and an independent start-on-exit
fallback. The durable request and any retained backup location are recorded in
`state_dir/maintenance/request.json`. After a killed helper, a surviving or
restarted bot reports interruption, not success. If automatic recovery cannot
start the bot, no Telegram result can be delivered until the operator restores
it; inspect the journal and saved backup. Executable rollback does not reverse
state/config migrations a future release may have made. A power failure or
forced helper kill is not a transactional rollback guarantee.

## Upgrade or remove the unit

For an archive release, extract the new package matching your architecture in a
download directory. Wait for active tasks, then stop the service, replace the
installed executable from that directory, and restart:

```sh
systemctl --user stop q-agent.service
install -D -m 0755 ./q-agent "$HOME/.local/bin/q-agent"
systemctl --user start q-agent.service
```

Keep your existing configuration and state; do not replace them with the
archive's example configuration. The updater in v0.1.0 expects raw binary assets
and cannot install the `.tar.gz` release packages, so use this manual procedure.
Newer updaters containing archive support accept either the raw binary or
`q-agent_<tag>_linux_<arch>.tar.gz`, extracting only the top-level executable.
Once this administration feature is installed and enabled, `/update` automates
the stop, install, readiness check and recovery sequence above.

A binary/config change at the same path needs a restart, not a daemon reload.
A unit-file change needs `daemon-reload` too. If moving the binary/config, review
`service print`, stop the old service, move the old unit aside, and reinstall.
The installer never overwrites a customized unit. Disabling is non-destructive:

```sh
systemctl --user disable --now q-agent.service
```

Move/remove the installed unit and reload only if you want to remove it; no
command here deletes config, credentials, state, chat files or base images.

## Offline validation

Ordinary tests exercise safe installation, config preservation, quoting, and the
real `systemd-analyze --user verify` parser when available. To run an opt-in test
against your running user manager:

```sh
make build
Q_AGENT_SYSTEMD_INTEGRATION=1 Q_AGENT_WORKER="$PWD/bin/q-agent" \
  go test -v ./internal/service -run TestIntegrationSystemdHostAndSandbox -timeout 2m
```

This creates uniquely named **temporary runtime units**, checks they don't
already exist, runs offline host and real rootless sandbox read/write probes,
and removes those units afterwards. It also verifies credential-file lookup,
`service.env` loading, and paths containing spaces and special characters.
The sandbox probe uses a minimal tar and the actual static worker/mapping helpers,
not a downloaded Debian image. It does not validate mmdebstrap/package setup,
contact Telegram/providers, or touch `q-agent.service` or its state.

The maintenance controller has a separate opt-in real-systemd test:

```sh
Q_AGENT_MAINTENANCE_SYSTEMD_TEST=1 go test -count=1 -v ./internal/maintenance \
  -run TestSystemdMaintenanceSurvivesBotCgroupStop -timeout 1m
```

It restarts an offline disposable probe through the production controller and
checks a new MainPID, a delivered local notice, private request state, and helper
survival after the probe's cgroup is stopped. It does not contact Telegram or
GitHub, install a release, or touch the real `q-agent.service`.

To validate the joined Telegram-to-systemd lifecycle, including archive upgrades
and failed-start rollback, run:

```sh
Q_AGENT_LIFECYCLE_SYSTEMD_TEST=1 go test -count=1 -v ./cmd/q-agent \
  -run '^TestIntegrationMaintenanceLifecycle$' -timeout 5m
```

Run as a non-root account with a running user manager, `go`, `bubblewrap`,
`newuidmap`/`newgidmap`, and working subordinate ID allocations. The test builds
two static, test-only executables with different versions. It runs restart,
upgrade and rollback cases in **both host and real rootless sandbox modes**,
using the production serve loop, Telegram transport, updater, independent helper
and persistent outbox. Each case checks tool read/write before and after a new
MainPID, persisted workspace contents, the reported version, installed executable
bytes, and preserved configuration/credential files. Sandbox cases use a minimal
local tar and the same executable as the service and worker.

Telegram, model and Release APIs are loopback HTTP fixtures; no real bot, provider
or hosted Release is contacted. A test-only controller remaps all service
operations, including the helper's start-on-exit fallback, to uniquely named
temporary runtime units and removes them afterwards. The rollback fixture is a
valid native static executable that deliberately fails startup. The test does
not install/start `q-agent.service`, publish a Release, download Debian packages,
or prove a live Telegram/hosted GitHub upgrade. Ordinary `go test ./...` skips
this opt-in test, so CI success alone is not evidence it ran.
