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
lease expires. A saved search result, source or successful commit can
be reused. A charged search or fetch with an unknown outcome stops as
`UNRESOLVED`. The worker follows the same rule for a retry whose
reservation was saved but whose result was interrupted. The manual
entry point runs in the foreground.

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

## Apply publication and authority rules

Set an optional `source_policy` in the JSON sent to `learning configure`.
Keep the rest of your learning policy in the same input.

```json
{
  "enabled": true,
  "permissions": {
    "allow_domains": ["records.example.org", "reference.example.net"]
  },
  "source_policy": {
    "max_age_days": 30,
    "domains": {
      "records.example.org": {
        "max_age_days": 90,
        "single_authority": true
      }
    }
  }
}
```

A positive age accepts declarations dated on or after the UTC calendar day
that many days ago, up to the fetch time. Missing, malformed, conflicting
and future declarations are ineligible when an age applies. The fetcher
reads `article:published_time` and `datePublished` metadata declared in the
HTML head. Dates are publisher declarations. An omitted age or zero disables
the date requirement. A rule for an exact host can override the default
age, including by setting it to zero. Host rules use lowercase DNS names.
The existing allowlist and blocklist still govern every source request.

An allowed host with `single_authority: true` can satisfy a two-source plan
with one date-eligible source and a matching, verifiable excerpt. Status
reports `authority_source_recorded` with coverage 0.5, the ratio of one
verified excerpt to the original two-source target. The worker stores a
reference note and its citation. The policy does not generate a Claim.
Each invocation uses the current policy to check saved sources before
reuse and commit. `learning trigger` shows the configured rules in the
offline plan preview.


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

## Export research transition audit

```sh
printf '%s\n' "$run_id" | q-agent learning audit --config ./config.toml
```

`enqueue`, `work`, `pause` and `resume` publish checkpointed transitions to
`<state_dir>/learning/audit/<event_id>.json` after updating run state.
`audit` replays a durable snapshot and prints its transitions in order. It
also backfills older runs and repairs missing files after an interruption.
Repeated exports use the same immutable event IDs. The event contains run
and plan IDs, timestamps, state, a known reason code and any recorded budget
usage. If automatic export fails, the command reports it on stderr and
preserves the checkpoint and its normal result. Run `audit` again after
resolving the storage error.

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

## Draft a claim from verified excerpts

```sh
printf '%s\n' "$run_id" | q-agent learning work --claims --config ./config.toml
```

`--claims` uses the configured local Ollama model and the same token,
deadline, retry, and audit limits as excerpt summaries. It accepts one or
two verified excerpts and sends their text in order. The host maps the
returned `supports`, `refutes` and `context` labels to the existing
Evidence IDs and source locators. A draft with support is stored as a
`hypothesis`. Supporting and refuting evidence together make it
`disputed`, with citations on both sides. A claim with refutation alone
remains `unresolved`. If there is no direct evidence or the
model abstains, the run ends `UNRESOLVED` with its checkpoint and audit.

A successful commit writes the draft, evidence and reference note
together. Work and status report the `claim_id` after the commit.
Use the same `--claims` flag when resuming before the model stage. A
saved draft can finish committing without another model call. The
checkpoint also carries the open question ID and accepted source
fingerprints for future research.

### Review and inspect claim history

```sh
printf '%s\n' "$claim_id" | q-agent learning claim-history --config ./config.toml
printf '%s\n' '{"claim_id":"CLAIM_ID","expected_revision":1,"action":"review","reason":"Checked the independent citations"}' |
  q-agent learning claim-edit --config ./config.toml
```

Research may add independent evidence to a uniquely matched, unscoped
proposition. A match uses normalized subject, predicate, and object and
can use an operator-recorded subject alias. Copies and reprints do not
reinforce a claim. Ambiguous matches remain separate drafts. Supporting
and refuting evidence together mark a claim disputed and keep both sets
of citations. Confidence uses independent, direct, fresh, public-reference
evidence and records the counts that inform the score.

The local edit commands require the current revision and a reason.
`review` can promote a hypothesis with two fresh independent supporting
sources. `qualify` replaces the alias and qualifier lists and accepts
`valid_from` and `valid_to` as RFC 3339 timestamps for when the claim
applies. `contradict` accepts `refuting_evidence_ids` that the operator
has checked against the claim. `stale` marks an approved claim for
revalidation. `supersede` accepts a supported `replacement_id` for the
same subject and predicate. `rollback` restores the previous wording and
scope as a new revision and retains accumulated evidence. History returns
the current state, prior snapshots recorded by edits, and replacement
lineage. Legacy claims begin retaining complete snapshots at their first
tracked edit. Concurrent edits check the expected revision before writing.

## Find previously read sources

```sh
printf '%s\n' '藤原氏 關白' | q-agent learning recall --config ./config.toml
```

Recall ranks matching public notes by keywords in titles, summaries,
verified excerpts and cited claims. It returns up to five matches with
sources, dates, byte locators and available claim statuses, applying
domain and text policies before ranking. An unrelated query yields an
empty array. Retrieval reads the local snapshot and uses no model or
auxiliary index files.

When available, topic names, aliases, taxonomy categories, and one-hop
topic links with verified Evidence also contribute to ranking. A snapshot
with only notes and citations uses the same keyword lookup. Publication
and fetch dates remain attached to the citations.

Ask “what did you research?” or “你最近研究了什麼？” for the latest
authorized notes. The CLI and chat accept `site:example.org`,
`after:2026-01-01` and `before:2027-01-01` filters. The first date is
inclusive and the second exclusive. Filters use the published UTC date
when available and fall back to the UTC fetch date.

For automatic recall in interactive Telegram chat, add the following
object to the learning policy, keep learning and memory enabled, and
restart the service.

```json
{"recall":{"chat_enabled":true,"audit_enabled":true}}
```

Recall uses only the public knowledge partition. Private chats must
be authorized and groups also need `permissions.include_group_chats`.
The active chat provider receives up to two matching notes as untrusted
prior research, with instructions to cite the supplied URLs and dates
and distinguish prior research from dialogue memory and live checks.
Unreviewed or unresolved claims retain their status. Sources outside a
configured publication-age rule carry an age warning.

Both options default to off. Optional `RecallUsed` events record the
note and claim IDs included in an answered turn. The audit contains IDs
and timestamps only. Retrieval and audit errors allow chat to continue.

### Read the Markdown body of a note

```sh
printf '%s\n' "$body_ref" | q-agent learning note --config ./config.toml
```

The command accepts a `note:<id>` body reference or a note ID returned by
recall. It renders the current summary, draft status and full citations
from the authorized knowledge snapshot. Re-rendering leaves the stored
note, claim and evidence IDs intact, and the Markdown view applies the same
source and text rules as chat recall. Recall supplies a body reference for
older notes with a learning run ID, without rewriting them.

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

```sh
q-agent learning frontier --public --select --config ./config.toml
q-agent learning frontier --public --enqueue next --config ./config.toml
```

The public frontier derives gaps from cited hypotheses, unresolved,
disputed, or stale claims. It derives associations from topic relationships
with verified citations. Each candidate records the claim or relationship
ID and its evidence IDs. Reviewing a supported claim creates an edge from
its subject to its object with the claim's predicate, qualifiers and
supporting excerpts. A two-edge association records the shared topic,
both edge IDs and evidence from each edge. Revising or downgrading a
claim updates which edges can produce candidates. Only policy-allowed
public knowledge feeds this path. Use `--origin gap` or `--origin association` on a list,
and `--status all` to include history for currently visible candidates.

`--enqueue next` queues one selected public lead. For either frontier,
`--enqueue <candidate-id>` queues a particular selected lead. For a
chat-scoped lead, pass an independently reviewed public question on
stdin. The queued run stores that question. The original chat text,
candidate origin references, and selection history stay in the original
topic partition.

The result includes a run ID for `q-agent learning work`. Queuing is local
and starts no provider call. Each run requests two verified sources
under the existing budgets. Repeating an explicit candidate ID returns
the same run, including after interruption between the run and frontier
writes. A later list refreshes finished runs into frontier history, and
identical questions stay suppressed while queued or in cooldown. Frontier
scope does not change the status of a cited claim.
Each shortlist is limited to five topics and depth two by default. When
an eligible gap exists and no gap has entered the queue in the previous
seven days, a slot gives it preference after manual choices. The
`selection` configuration accepts `pinned_ids`, `muted_ids`,
`banned_topic_ids`, and `priority_overrides`. Pins and positive priority
take precedence over quota ranking after policy and cooldown checks.
The result explains each selection and does not change queue status.

## Run approved research overnight

Enable the learning policy and set a schedule. For example, include this
schedule in the JSON sent to `learning configure` and restart the service.

```json
{"enabled":true,"schedule":{"enabled":true,"timezone":"Asia/Taipei","window_start":"02:00","window_end":"05:00","idle_minutes":30}}
```

Preserve other settings when configuring the policy. Keep memory enabled,
allow the search service and the intended public source domains, and
enable chat recall to use completed research in Telegram.

```sh
printf '%s\n' "$run_id" | q-agent learning schedule --config ./config.toml
```

Supply a queued run ID from `learning enqueue` or `learning frontier
--enqueue`. Scheduled work uses the approved public question already in
that run. A chat candidate still requires an operator-reviewed public
question at the frontier handoff. The overnight queue accepts one pending
run and starts at most one per local window. It dispatches only after the
configured idle interval when no interactive or scheduled chat task is
active or queued. Work uses existing source policy and budgets and stores
a cited note for recall. The scheduler does not send a Telegram message.

The service checks the current policy before dispatch, so disabling the
schedule also blocks dispatch. Restart the service to enable scheduling or
move the window outside the startup policy's hours. An interrupted run keeps its checkpoint and releases its lease. When
the scheduled time limit expires, the active approval carries to a
later window. The worker checks current budget and source policy before
continuing, and a provider request with an uncertain outcome is not
automatically repeated. The checkpoint and commit receipt prevent
duplicate notes and Claim versions. To withdraw approval,
pass the same ID to `learning schedule --remove` when the learning
directory lock is available.

### Recheck sources overnight

Set `source_policy.revalidate_days` to a positive number of days in the
learning policy to check the public sources behind supported Claims.
This applies to the oldest due source, at most one per idle window with
no active or pending approved research. The check uses the existing
allowlist, robots rules and daily request and document quota. It never
uses search or a model. Zero or omission keeps revalidation off.

A matching document extends the verification timestamp. Changed bytes,
HTTP 404 or 410, a document-level opt-out, or a failed publication-date
rule mark that version unavailable and record a stale revision for each
supported Claim citing it as support. Transient or inconclusive results
leave Claim status intact. Original citations remain available as
historical evidence, and stale Claims can appear as gap candidates in
`learning frontier --public`. The check compares full document hashes,
so a formatting-only change can also trigger review. A new research run
can supply a fresh cited version for the next operator review.

For an optional feedback signal, enable `recall.frontier_feedback` with
chat recall and recall audit. The most recent audit event with Claim IDs
adds a small local utility bonus to related candidates for up to seven
days. This uses one bounded metadata record and never changes origin
quotas or sends recall data to a search provider.

For M3 drift monitoring, aggregate the existing revalidation outcomes
and stale-to-reviewed revisions by source domain. Compare consecutive
weekly rates for changed bytes, unavailable sources and unresolved gaps
before adding any automatic intervention. The scheduled worker already
preserves open questions, fingerprints and budget for a later window;
a multi-night program can build on the same checkpoint and approval
boundary.

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
bounds before dispatch. A worker can retry each read-only search or fetch
once after HTTP 502, 503 or 504 when the response has no `Retry-After`.
The retry reserves its full call cost and a 250 ms cancellable backoff
before waiting. A fetch retry repeats the robots check and the document
request under the same access policy. Rate limits, access denials and
unknown transport outcomes end the attempt. The standalone `learning
search` command still makes one request. The structured-output
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
  fence. Provider dispatch enforces budget reservations, cancellation,
  deadlines, and one retry for eligible HTTP responses. The manual worker
  adds checkpointed collection and a
  fenced reference-note commit and replays durable transitions into the
  shared audit log. The planner applies publication age, exact-host
  overrides, and an opt-in single-authority rule. Optional claim drafting
  associates verified evidence with a hypothesis, disputed claim, or
  unresolved draft.
- #73 owns note and claim recall. Telegram and the CLI can retrieve
  authorized cited notes with keywords and date/domain filters. Chat
  applies a shared context budget and can emit metadata-only recall events.
- #74 provides atomic claim revisions, evidence-based conflict handling,
  explicit review, and replacement history.
- #76 owns further provider egress controls, ACL for remote controls, and
  deletion across durable knowledge, indexes and backups. Nightly execution
  accepts explicitly approved public run IDs.
