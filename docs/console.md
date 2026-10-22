# Console: for people and agents

[← Overview](../README.md)

Use **a command group on its own for a guided menu**. Add **an action for a
prompt-free command** that scripts and agents can use. The same configuration
file powers both paths.

```sh
# Human: open a menu and review changes before saving
q-agent models
q-agent config

# Agent/script: execute exactly one action without questions
q-agent models show --json
q-agent models set-default --model qwen3:8b
q-agent config get limits.max_steps --json
q-agent config set limits.max_steps 12
```

Examples assume `q-agent` is on PATH; from a source checkout use
`./bin/q-agent` instead. Build with `make build` first.

## The interaction contract

- `models` and `config` with no action require terminal input. Redirected input,
  pipes, and non-TTY agent sessions get an error immediately, never a hidden prompt.
- Explicit actions **never prompt**, including when arguments are missing or
  invalid. Errors go to stderr and produce a nonzero exit status.
- `--json` makes successful output a single JSON value on stdout. Errors remain
  text on stderr. A bare group plus `--json` is an error; name the action instead.
- `--help`, `help`, or no arguments displays the command reference without
  loading configuration. `models --help` and `config --help` do the same.
- The existing `serve`, `image build`, `version`, and internal `worker` execution
  paths remain non-interactive. A config edit never starts or restarts a service.

## Choose your configuration

The default is `~/.config/q-agent/config.toml`. For either path, select
another existing file with `--config FILE`:

```sh
q-agent models --config ./my-bot.toml
q-agent --help
q-agent config --config ./my-bot.toml get provider.model --json
q-agent config get provider.model --config ./my-bot.toml --json
```

Console options belong after the top-level group (`models` or `config`), not
before it. `--config` and `--json` can appear before or after the action.
Use `--` to protect subsequent literal values beginning with `--`:

```sh
q-agent config --config config.toml set provider.model -- --custom-model
```

## Models

`q-agent models` opens a menu showing the default provider plus extra providers. Option 1 sets the default provider/model (optionally base URL and thinking). Option 2 manages extra `[providers.*]` entries (add/edit/remove). Option 3 lists models for a chosen provider, and option 4 shows supported adapters. Enter keeps the current value (or the new provider's default); `-` clears the optional base URL or thinking field. Saving requires an explicit `y` or `yes`.

| Action | Result |
| --- | --- |
| `models show [--provider NAME\|--all]` | Default provider settings, one extra provider, or all providers including model and credential references. |
| `models providers` | Supported adapters: Ollama, OpenAI, OpenCode Go; works without a config file. |
| `models list [--provider NAME]` | Model IDs advertised by the default (or named) provider, sorted by ID. |
| `models set-default ...` | Validate and save default `[provider]` settings. |
| `models add/update/remove ...` | Add, edit, or delete a named `[providers.NAME]` extra. |
| `models set ...` | Deprecated alias for `set-default`. |

All `models` actions except the human menu support `--json`. `show --all` lists default plus extras; `show --provider NAME` and `list --provider NAME` target one provider.

```sh
q-agent models list --json
q-agent models list --provider openai --json
q-agent models show --all --json
q-agent models set-default --model qwen3:8b --thinking high --json
q-agent models set-default --provider openai --model YOUR_MODEL_ID --json
q-agent models add --provider openai --model YOUR_MODEL_ID --api-key-env OPENAI_API_KEY --json
q-agent models update --provider openai --model NEW_MODEL --json
q-agent models remove --provider openai --json
```

`models set-default` accepts `--provider`, `--model`, `--base-url`, `--thinking`,
`--api-key-env`, and `--api-key-path`. It edits only the default `[provider]` table. It needs at least one setting. A provider change also requires `--model` and clears the previous default's base URL, thinking policy, and credential references unless you explicitly supply new ones in the same command. Changing just the model keeps the other settings.

`models add --provider NAME --model NAME [...]` creates a new `[providers.NAME]` extra; adding does not change the default. `NAME` must be a supported adapter and must not duplicate the default. `models update --provider NAME [...]` edits an existing extra (at least one other setting; `--model` cannot be empty). `models remove --provider NAME` deletes the whole table. `models set --...` remains as a deprecated alias for `set-default` and prints a warning to stderr.

Selecting `--api-key-env` clears the file reference; selecting `--api-key-path`
clears the environment reference. Explicitly specifying both nonempty sources
fails validation. Pass **the variable name or file path**, never the key itself.
The console does not write credentials or resolve them when displaying/editing
configuration.

`models list [--provider NAME]` is the only console action that contacts a provider. It uses the
selected provider credential, has a 15-second overall timeout, and never calls
a completion API. Ollama may also query model metadata. It does not install
models, guarantee access to every advertised model, or validate the model's
thinking level. `models show` works offline; missing credentials do not prevent
configuration inspection or edits.

## Other settings

`q-agent config` shows effective settings with defaults, lets you edit one
setting at a time, and can validate the file. Use the dotted keys from the
TOML configuration, for example `provider.model`, `sandbox.enabled`, or
`telegram.allowed_groups`.

```sh
q-agent config show --json
q-agent config get provider.model --json
q-agent config validate --json
q-agent config set limits.task_timeout 10m
q-agent config set sandbox.enabled true
q-agent config set telegram.allowed_users '[123456789]'
q-agent config set telegram.allowed_groups '[-1001234567890]'
```

String values are plain text; integers, booleans, and ID arrays use TOML literals.
Shell-quote values containing spaces or brackets. Unknown keys, wrong types,
invalid limits/durations, and conflicting credential sources are rejected.
`config set` changes **only the named field** (including `providers.NAME.field` for extras); use `models set-default ...`
when you want default provider-specific settings reset together, or `models add/update/remove` for extras. Switching Telegram
credential sources via `config set` requires clearing the old reference first.

`config validate` checks file syntax and the same local settings rules as
startup. It does **not** check network access, credentials, Telegram allowlist
membership, sandbox dependencies, model availability, or service health.

## Saving and visibility

- These are **saved settings**, not a live runtime status endpoint. Use Telegram
  `/status` for the running bot's snapshot. Edits take effect the next time the
  service starts; restart it through your normal deployment process.
- Successful edits return `{"saved":true,"restart_required":true}` with `--json`.
- Edits preserve unrelated keys, comments, relative paths, and file permissions.
  A selected multiline value is replaced as a whole, including comments inside
  that value. Standard TOML tables, dotted keys, and quoted keys are supported;
  inline-table layouts that cannot be surgically updated fail without saving.
- A candidate is validated before atomic replacement. Invalid candidates leave
  the original file untouched. The file must already exist, be owner-writable,
  and not be a symlink. The CLI leaves a sibling `.lock` file for coordinating
  writers; it contains no configuration. It checks for external edits before
  replacement, but is not a general-purpose lock for text editors.
- EOF or cancellation before an affirmative menu confirmation saves nothing.
  Previously confirmed edits stay saved when you later exit the menu.
- On load, deprecated keys (`max_tool_calls`, old `approved_*` allowlist)
  are auto-migrated and the file rewritten with a timestamped backup kept
  next to it; the migration is reported on stderr, never on `--json` stdout.
  Files mixing old and new allowlist keys are left untouched with an error.
- Output includes credential **references**, not resolved values. URL userinfo,
  query strings, and fragments are omitted from displayed base URLs. Display
  output is therefore not a lossless config backup.

See [Configuration & operations](operations.md) for the complete setup and
credential-file permission rules.
