# Validation and initial performance baseline

Validated on 2026-10-10 with Go 1.23.12, Linux amd64,
WSL2 kernel 6.6.87.2-microsoft-standard-WSL2, an Intel i7-13650HX,
and an ext4 workspace. The implementation was extracted from qcode `e9389fa`;
qcode's working tree remained unchanged.

## Completed checks

| Check | Result |
| --- | --- |
| `go test ./...` | Passed |
| `go test -race ./...` | Passed |
| `go vet ./...` | Passed |
| `make build` (`CGO_ENABLED=0`, stripped native binary) | Passed |
| Cross-compilation for Darwin with CGO disabled | Passed |
| Rootless mmdebstrap image build with a clean environment | Passed |
| Real bubblewrap integration suite | Passed, 34.51 seconds |
| Full service integration with mocked Telegram/Ollama and real tools | Passed, 18.44 seconds |

Provider tests were ported from the pinned qcode commit, covering Chat
Completions, OpenCode's Messages and Responses routes, Ollama, usage and missing
usage, reasoning continuation, errors, and cancellation. New core tests cover
sequential tool round trips, canonical repeated-call detection, recovery notices,
limits, untrusted observations, credential redaction, and stopping immediately
on a failed execution boundary.

Adapter/runtime/state tests cover both allowlists (including commands), group
addressing, UTF-16 mention offsets, topic separation, Unicode reply splitting,
duplicate updates, provisioning counted against concurrency, busy replies,
cancellation, history bounds, `/new`, interrupted tasks after restart, persisted
provider identities, reply retries and chunk resumption, and failed destinations
not blocking other chats.

The live Linux sandbox tests exercised:

- Both UID/GID ranges and sandbox root, with effective capabilities `0x400db`.
- Two independent rootfs trees, persistent system/workspace edits, and all six tools.
- Cleared host credentials and inaccessible host paths, image mounts, and staging paths.
- A read-only worker and fresh `/tmp` and `/run` on later calls.
- Unicode output truncation, child-process termination, cancellation, and shell timeout.
- Installing `jq` in one workspace, using it in later calls and after reopening the
  workspace manager, and confirming it is absent from the other workspace.
- Mapping-helper failure before tool execution, rejected incomplete provisioning,
  and cleanup after an archive fails partway through extraction.

The full service test ran a long-poll update through authorization, an Ollama tool
continuation, actual sandbox execution, a topic-preserving reply, history and cursor
persistence, and a host-runtime restart without replaying completed agent work.
Telegram and model network endpoints were mocked; no live Telegram messages or
paid model requests were sent. Race checks cover the unit/runtime tests; the live
integration run used the normal static worker.

## Initial baseline (commit 97df202)

| Measurement | Observed value |
| --- | --- |
| Stripped static native binary | 6,492,312 bytes (6.19 MiB) |
| Idle full-service test process RSS | 10,720 KiB (10.47 MiB) |
| Rootless mmdebstrap base-image build | 16.36 seconds |
| Uncompressed base archive | 118,320,640 bytes (112.84 MiB) |
| First workspace creation, serial run | 10.42 seconds |
| First workspace creation, concurrent integration suites | 17.88 seconds |
| Diagnostic shell tool call, concurrent integration suites | 11.25 milliseconds |

RSS includes the Go test harness and in-process mock HTTP servers; it is an idle
runtime baseline, not a measurement of a deployed Telegram service. Workspace
creation includes per-file synchronization and publication. Tool latency includes
mapping helpers, bubblewrap, worker startup, and the diagnostic shell command.
These are initial observations on this machine, not latency distributions.

## Reproduction

```sh
make test
make race
make build
./bin/q-agent image build --config config.toml
Q_AGENT_IMAGE="$HOME/.local/share/q-agent/images/debian-trixie.tar" make integration
```

The validation image caches are local ignored artifacts under `.test-state/images/`.
The integration tests remove their own rootfs trees through the mapped worker.
`config.toml`, binaries, and validation image caches are excluded from Git.

## TOML configuration update

Configuration now uses `config.toml` and `config.example.toml`, parsed with the
same pinned `go-toml/v2` v2.3.1 dependency as qcode. The loader preserves defaults,
relative-path resolution, environment credential selection, duration checks, and
strict unknown-field validation. Tests cover comments, negative 64-bit chat IDs,
invalid syntax, duplicate keys, and oversized configuration files. Host state
continues to use atomic JSON files.

`go test ./...`, `go test -race ./...`, `go vet ./...`, and the static build passed.
The cached-image CLI command also passed using the local TOML validation config.
The updated stripped native binary is 6,693,016 bytes (6.38 MiB); the table above
records the original implementation's baseline.

## Pre-publication audit

All reachable Git commits and source blobs were checked with Gitleaks v8.30.1
and against credentials available in the local environment. No real credentials
were found. The scanner's four matches were manually reviewed synthetic
redaction fixtures; `.gitleaksignore` limits their exceptions to the original
commit, rule, and line. Private operator configuration, runtime state, images,
and binaries are untracked and ignored. Published commit metadata uses the
GitHub noreply address, and the isolation test uses a temporary host marker
instead of a workstation-specific path.

After the audit changes, unit tests, race checks, `go vet`, the static build,
and both live integration suites passed again. The host-isolation check now
asserts that a file which exists on the host is inaccessible inside the sandbox.

## Host execution default (2026-10-12)

Tools now execute on the host unless TOML explicitly sets `sandbox.enabled = true`.
The ignored local OpenCode configuration and the tracked example select host
execution. Host mode requires Linux and uses the service account's ordinary
permissions. Credential environment variables are filtered and their values join
the existing redaction policy. Host and sandbox files remain in separate trees.

Validated on the same Linux amd64 environment with Go 1.23.12:

| Check | Result |
| --- | --- |
| `go test ./...` | Passed, including mocked host service integration |
| `go test -race ./...` | Passed |
| `go vet ./...` | Passed |
| `make build` | Passed; stripped, statically linked ELF |
| `make integration-host` | Passed; host tools 3.46 seconds, service 0.30 seconds |
| Real sandbox isolation and package integration | Passed, 33.85 seconds |
| Mocked service with real sandbox tools | Passed, 18.46 seconds |
| Gitleaks v8.30.1 over all reachable Git history | Passed |
| Gitleaks publication scan of staged changes | Passed |

An exact-value audit checked 108 reachable/staged source blobs and commits.
The validation process had no configured or recognized credential values in its
environment, so that comparison had no credential values to check.

Configuration tests cover omitted, false, true, and invalid `enabled` values,
including old sandbox settings that must not implicitly enable sandboxing.
The mocked host service starts with nonexistent image, worker, and mapping-helper
paths and an incompatible legacy sandbox mapping. Explicit sandbox startup still
fails on invalid setup before contacting Telegram and never falls back to host.

Host tests exercise all six tools, relative and absolute host paths, separate
chat folders, shared topic folders, restart persistence, service UID/GID,
credential filtering, retained ordinary environment values, concurrent working
directories and environments, Unicode output limits, shell deadlines,
cancellation, and cleanup of ordinary background processes. Tool names and
argument shapes remain unchanged. Host mode cannot guarantee termination of
children that deliberately detach from the command's process group.

Mode-state tests exercise both transitions across all topics, preservation of
files/cursor/queued reply progress, legacy sandbox history, and same-mode identity
and history persistence. Injected failures after partial clearing and after all
histories are cleared leave the prior mode uncommitted and repeat the reset on
restart. Service tests verify failed resets stop startup before network or agent
work. Sandbox integrations explicitly enable sandbox mode and retain their
isolation, provisioning-failure, process-cleanup, and package-persistence checks.

All Telegram and provider endpoints in service tests were mocked. No live messages
or paid model requests were sent, and qcode's working tree remained unchanged.

## Persistent scheduler (2026-10-17)

Added deterministic store/runtime coverage for durable due admission and
idempotency, five-minute grace boundaries, long-downtime daily/weekly coalescing,
local-time/DST recurrence, pending/running restart recovery, scheduled reply
chunk resumption, failed atomic-write rollback, chat/topic/user ownership,
delete/cancel/authorization revocation, workspace/global concurrency limits,
worker-completion wakeups, no-LLM reminders, failed-job non-retry, queue
backpressure, terminal-history retention, bounded list pagination, and fairness
between scheduled and ordinary reply queues.

Validation on the implementation host:

- `go test ./...`: passed.
- `go vet ./...`: passed.
- `make build`: passed.
- `make integration-host`: passed (mock Telegram/Ollama, real host tools).
- `go test -race ./...`: unavailable on this host; every instrumented binary
  exited before tests with `ThreadSanitizer: unsupported VMA range` / `Found 47 -
  Supported 48`. This is not a passing race check; rerun on a supported host.
- Full rootless sandbox integration was not run for this change. Existing
  ordinary sandbox unit tests passed; scheduler tools execute in the host and
  preserve the wrapped runner's execution-mode system prompt.

Tests create local temporary state and send no live Telegram messages. No
production service, configuration, or schedule was changed during validation.

## Web fetch and DuckDuckGo search (2026-10-18)

Started from a fast-forward sync of `main` at `d410b3d`. Validated on Linux
arm64 with Go 1.24.4:

- Full `go test -count=1 -timeout 5m ./...`, `go vet ./...`, formatting,
  `git diff --check`, and static `make build`: passed.
- Deterministic web tests cover HTTP/HTTPS, standard TLS verification, readable
  HTML and relative links, plain text/JSON, character encodings, gzip and input/
  UTF-8 output limits, URL validation, redirect limits, cancellation, recoverable
  site timeouts, DuckDuckGo Lite/HTML results, URL unwrapping, duplicate filtering,
  requested result count, bot challenges, HTTP failures, and unknown page formats.
- Live host-side search returned real DuckDuckGo results, including `https://go.dev/`;
  HTTPS fetching of that site passed.
- A real static sandbox worker passed HTTP fetching, argument validation, live
  DuckDuckGo search and verified HTTPS fetching. This test uses a temporary
  minimal rootfs with public CA certificates and DNS settings, real bubblewrap,
  subordinate UID/GID mapping, and worker cleanup; it does not substitute mocked
  sandbox execution or change a deployed image. The full Debian/package-install
  integration suite was not rerun for this change.

Reproduce the opt-in live checks (ordinary CI remains offline):

```sh
Q_AGENT_WEB_LIVE_TEST=1 go test -v ./internal/localtools -run '^TestWebLive$' -count=1
make build
Q_AGENT_WEB_INTEGRATION=1 Q_AGENT_WEB_LIVE_TEST=1 \
  Q_AGENT_WORKER="$PWD/bin/q-agent" \
  go test -v ./internal/sandbox -run '^TestIntegrationWebTools$' -count=1 -timeout 2m
```

Omit `Q_AGENT_WEB_LIVE_TEST=1` in the sandbox command for a local HTTP-only
check without external requests or a Debian image. Live checks may fail when
DuckDuckGo rate-limits the host or presents a challenge. No live Telegram messages
or model requests were sent, and no production configuration/service was changed.
