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
