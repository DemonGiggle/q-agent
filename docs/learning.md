# Autonomous Curious Agent integration

ACA is opt-in. The M0 entry points run as a local operator and store their
policy and audit metadata under `<state_dir>/learning/`. Normal chat startup
and delivery do not depend on ACA state.

## Preview a plan

```sh
printf '%s\n' '{"enabled":true}' | q-agent learning configure --config ./config.toml
printf '%s\n' 'What sustained Fujiwara influence during the Heian period?' |
  q-agent learning trigger --config ./config.toml
```

`configure` takes one JSON object and fills omitted fields from the defaults.
Its output contains the effective policy. Learning and scheduling default to
off. The time window uses IANA timezone names and accepts windows that cross
midnight. Daily, run, and question budgets constrain requests, documents,
tokens, USD millionths, milliseconds, depth, and stored bytes.

`trigger` is the local `learning.trigger(dry_run)` entry point. It reads a
manual question from standard input, applies the exclusion list and secret
redaction, and returns a question, a template plan, and a planned run ID.
It makes zero provider calls, writes no knowledge records, and creates no
scheduled work. Each successful invocation appends a separate
`ResearchPlanned` event with generated references. Audit contains no question
or transcript text. The audit sink rejects changes to an existing event ID.

## Discover sources manually

```sh
printf '%s\n' 'Fujiwara regency Heian period' |
  q-agent learning search --config ./config.toml
```

The explicit search command sends the supplied query to DuckDuckGo Lite
and returns up to five unverified hits. It accepts up to 2,000 bytes and
rejects detected credentials and excluded terms before dispatch. Optional
allow/block lists apply to the provider domain and result domains. This
command never reads conversation history.

Each search reserves one outbound request and up to 15 seconds of wall
time. It does not follow redirects or fetch the result URLs. The run
finishes as `UNRESOLVED` with reason `discovery_only`, leaving claims
and evidence outside the knowledge store until they can be verified.
Search holds the learning state directory's single-owner lock, independent
of the chat service lock. The provider can report an upstream error or
challenge and the request reservation remains charged.

## Run one queued research candidate

```sh
printf '%s\n' '{"enabled":true,"permissions":{"allow_domains":["lite.duckduckgo.com","example.org"]}}' |
  q-agent learning configure --config ./config.toml
printf '%s\n' 'Public example documents' |
  q-agent learning enqueue --config ./config.toml
printf '%s\n' "$run_id" | q-agent learning work --config ./config.toml
```

Copy the enqueue response's `run_id` into `run_id` for the work command.
Enqueue persists an explicitly public operator-supplied question without
contacting a provider. Work acquires that candidate's lease and rechecks
policy before sending its question. The worker searches once and attempts
to retrieve the first eligible result. Source retrieval requires an
explicit allowlist, HTTPS on the standard port, and public IPv4. It
rejects redirects, query parameters, and documents over 128 KiB. Sites
with a recognized robots file containing exclusions, delays, or unknown
directives are skipped conservatively. Document opt-outs also prevent
recording.

Work records the run state, transition timestamps and stop reason. A
successful run stores fetched source metadata and a reference note in
the public partition. When a passage matches the question's keywords,
the same commit includes one Evidence record of up to 240 UTF-8 bytes.
The reader uses plain text and contiguous HTML paragraph, list item, or
blockquote text. It skips hidden sections, snippet opt-outs, and text
that requires entity decoding. The document body and search snippet stay
outside durable storage.

The `source-bytes-v1` locator uses zero-based byte offsets with an exclusive
end. To verify a citation, match the SHA-256 hash of the fetched response
body against the stored source hash, then compare the indicated bytes to
the saved excerpt. When present, the work response includes the
`evidence_id`. The excerpt preserves provenance. Claim generation and
cross-source verification are later stages.

To resume after a crash, invoke work with the same run ID after the
lease expires. A saved source or successful commit can be reused. A
charged call with no saved outcome stops as `UNRESOLVED`. The worker
does not repeat that uncertain call. This manual entry point starts no
scheduler or idle task.

## Cross-check collection with two sources

Pass `--sources 2` to `learning enqueue` to require two distinct public
sources with verified excerpts. The target stays with the run. The worker
considers up to three registrable domains in the returned search results.
It rejects shared domains, shared declared canonical origins, identical
bodies and identical normalized excerpts. These are conservative duplicate
checks. Establishing factual agreement and publisher relationships still
requires further review.

Two-source runs checkpoint each attempted fetch. A completed run commits
both excerpts in the same transaction. If collection stops early, partial
coverage remains available in status and the knowledge store receives no
partial note. The default single-source path still supports
`learning work --summarize`.


## Inspect a research run

```sh
printf '%s\n' "$run_id" | q-agent learning status --config ./config.toml
```

The command reads a public research run without taking a lease or calling a
provider. It returns the stop reason, reserved usage and ordered transition
metadata. Transition event IDs stay stable across reads and worker restarts.
New transitions record usage in the same atomic checkpoint as the state
change, including budget exhaustion and cancellation.

Coverage is the number of sources with verified excerpts divided by the
plan's required source count. A two-source run with one verified excerpt
reports 0.5 until it can verify the second.
Coverage measures collection and does not rate claim confidence.

## Pause between worker invocations

```sh
printf '%s\n' "$run_id" | q-agent learning pause --config ./config.toml
printf '%s\n' "$run_id" | q-agent learning resume --config ./config.toml
```

Pause preserves the checkpoint, reserved usage and prior execution state.
Resume restores that state for a subsequent `learning work` invocation.
Repeated commands are idempotent and a paused run cannot acquire a lease.
These controls use the learning directory lock, so an active worker must
release its lock before an operator can pause. A run whose commit has
already begun keeps its normal commit recovery path. If a provider
response was never checkpointed, resuming still follows the rule against
replaying uncertain calls.


## Summarize a public excerpt

Use the normal research run ID with
`q-agent learning work --summarize --config ./config.toml`. This flag
uses the default `[provider]` only when it is an installed Ollama model
served at an HTTP(S) loopback IP. The q-agent process sends only the
validated public excerpt and a fixed summarization instruction to Ollama.

Each request reserves 2,432 tokens and up to 15 seconds. The adapter
requests a 2,048-token context and at most 384 generated tokens, requires
token counters on the response, and releases the Ollama model after the
call. An invalid JSON response can retry once under a separate reservation.
Every admitted attempt is recorded before dispatch as a metadata-only
`ModelCallReserved` event. The model summary is labeled unreviewed and
linked to the existing excerpt, with no Claim created. Model failures
leave the ordinary source note available. Budget exhaustion stops the
run. A resumed run never replays an uncertain model request.

## Find previously read sources

```sh
printf '%s\n' '藤原氏 關白' | q-agent learning recall --config ./config.toml
```

Recall ranks matching public notes by keywords in titles, summaries and
verified excerpts. It returns up to five matches with their source URLs,
fetch dates and excerpt locators, applying the current domain and text
policies before ranking. No matches produces an empty array. The command
reads the current snapshot on demand and uses no model or auxiliary index
files. Telegram recall is a separate integration.

## Collect local research candidates

```sh
q-agent learning frontier --chat-id -1001234567890 --topic-id 7 --config ./config.toml
```

Enable learning and the relevant chat type in the learning policy. The
requested chat must also be authorized by the Telegram service configuration.
The command uses up to 24 completed exchanges from that topic's last 48
hours. Explicit user questions produce related research candidates with
origin references and a deterministic score. Repeated and near-duplicate
questions reuse the same entry. The frontier stores up to 64 candidates per
topic. Newly excluded text is withheld from results.

Add `--select` to show a shortlist under the current selection policy.
The default targets are 70% conversation, 20% knowledge gaps and 10%
associations. Empty categories cede their slots to other eligible origins.
Each shortlist is limited to five topics and depth two by default. When
an eligible gap exists and no gap has entered the queue in the previous
seven days, a slot gives it preference after manual choices. The
`selection` configuration accepts `pinned_ids`, `muted_ids`,
`banned_topic_ids`, and `priority_overrides`. Pins and positive priority
take precedence over quota ranking after policy and cooldown checks.
The result explains each selection and does not change queue status.

Candidates stay in their private topic partition. This local command
does not invoke a provider or transfer a candidate to a public research
run. Automatic scheduling and approved egress are separate steps.

## Resource constraints

Keep ACA on the same binary and reuse existing state, provider and
scheduler infrastructure. The default chat path does not start a learning
worker, issue provider calls, or write learning metadata. The service reads
the optional policy once at startup when memory is enabled. Short-history
origin markers are added only for chat types explicitly enabled in the
policy. Restart the service after changing those capture settings.

Start with bounded local processing and keyword retrieval. Keep network
and research work out of the chat path, and add optional indexes only when
measurements justify them. For architecture PRs, compare stripped release
binary size and report any change to idle/background work or chat-path I/O.
Measure relevant query latency or allocation changes when adding live
recall or research processing.

## Component contracts

| Component | Boundary |
| --- | --- |
| Conversation reader | A host-authorized query supplies platform, chat, topic, visibility, and a half-open time window. The runtime binds it to an authorized origin and requires a separate opt-in for private or group chat. |
| Knowledge repository | The authorized partition is the unit of versioned updates. Claims, provenance, revisions, and idempotency receipts must change atomically. A stale writer must fail before persisting knowledge. |
| Index | Indexes derive from the authoritative partition and can be rebuilt. A keyword implementation can satisfy the basic retrieval contract. |
| Search, fetch, reader, and model | Search yields discovery references. A reader supplies locators in fetched documents. A structured model call receives reference data with no privileged tool executor. Structured results need validation before they influence run state or knowledge. |
| Scheduler and coordinator | The scheduler supplies wakeups. The coordinator owns leases, fencing tokens, checkpoint versions, cancellation, and release. Budget exhaustion and retries need durable run state. |
| Audit | Events use stable IDs and run correlation. Audit writes contain bounded metadata and keep raw documents and private chat outside telemetry. |

The reader uses the journal and reads recent short history for an authorized
topic when the digest has consumed its journal entries. It returns only
completed interactive exchanges, excludes group overhear, scheduled runs,
and failure diagnostics, and deduplicates both sources by origin. Legacy
short history without provenance is skipped. Clearing chat memory increments
the epoch so the retained short history cannot repopulate learning from old
turns. The scope identifies where the exchange was recorded. Final replies
may have incorporated chat-wide memory. Provider export therefore needs its
own policy check before any conversation-derived text can leave the host.

## Local knowledge store

Each authorized partition uses one JSON file under
`<state_dir>/learning/knowledge/`. The initial cap is 4 MiB per file,
including retry receipts and revision events. Reads and replacements are
bounded by this cap. The repository has no background process, index, or
new dependency. It is constructed lazily within the single-owner
learning state directory.

A write must hold the coordinator's current lease guard, match the
partition version, and validate citations in the replacement snapshot.
The snapshot and idempotency receipt are installed with a single synced
rename. A repeated operation with the same payload and run returns its
original commit version. Reusing an operation ID for different content or
committing from a stale version fails. Reaching the cap rejects the write
and preserves the last committed version. Retention policy and full Claim
history belong to their respective follow-up issues.

## Durable run coordination

Runs use one JSON file each under `<state_dir>/learning/runs/`, with a 64 KiB
cap for the run and its last checkpoint. The coordinator uses the existing
single-owner state lock and separate synchronization from chat. It performs
I/O on demand and checks lease expiry when handling requests.

Run transitions compare the checkpoint version, and a replacement lease
increments a persisted fencing token. Before entering COMMITTING, persist an
operation ID in the checkpoint. Only that operation can write to the bound
knowledge partition while the lease is current. Cancellation and the write
share the same coordinator lock. If cancellation wins, the lease is revoked.
If the write wins, its receipt survives a restart and the resumed run can
mark itself complete using the same operation ID.

Provider calls reserve their upper bound against plan, run, question and
global daily budgets before sending a request. The 64 KiB quota ledger
holds global and per-question usage for the current UTC day. The run
checkpoint also enforces the run's cumulative allowance across days. The
ledger is written before the checkpoint, and both writes must succeed
before the provider is called. A crash between writes can consume some of
the daily allowance even if the call never starts. Reservations remain
charged on error or cancellation to account for uncertain provider usage.

Calls run with a deadline capped by the active lease and UTC day. A run
has one in-flight call, and cancelling or releasing its lease interrupts the
call context. Adapters must honor context and enforce their declared upper
bounds before dispatch. The provider call path introduces no permanent
timer, polling loop or background goroutine. The structured-output
boundary validates against a host schema using the existing JSON Schema
library. An invalid output permits one separately budgeted retry. Provider
and cancellation errors stop immediately. Additional research events and
multi-source synthesis are follow-up work.

## Follow-up issues

- #65 / #69 / #70 own the integration contract. This slice includes the
  reader, domain types, config validation, metadata audit, and the offline
  preview. The local repository validates stored citation references.
  The manual worker connects search, fetch, source reading, and the local
  structured model adapter. Recall uses the keyword index.
- #72 has durable leases, cancellation, checkpoints and a tested commit
  fence. Provider dispatch now enforces budget reservations, cancellation,
  and deadlines. The manual worker adds checkpointed collection and a
  fenced reference-note commit. Evidence synthesis and centralized audit
  integration stay open.
- #73 owns chat recall and its share of the context budget. Recall uses
  authorized evidence and a separate failure path from normal chat.
- #74 owns Claim lifecycle and atomic Revision history. Use evidence links
  to record support/refutation per Claim, and keep prior versions queryable.
- #76 owns provider egress, ACL for any remote controls, and deletion across
  durable knowledge/indexes/backups. Conversation-derived egress and
  scheduling need their own policy controls.
