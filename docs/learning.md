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

## Follow-up issues

- #65 / #69 / #70 own the integration contract. This slice includes the
  reader, domain types, config validation, metadata audit, and the offline
  preview. Live adapter wiring, citation validation against stored records,
  and repository/coordinator implementations follow.
- #72 owns durable leases, budget accounting, cancellation, and checkpoint
  transitions. Define the commit fence and test a retry between commit and
  checkpoint before enabling a worker.
- #73 owns chat recall and its share of the context budget. Recall uses
  authorized evidence and a separate failure path from normal chat.
- #74 owns Claim lifecycle and atomic Revision history. Use evidence links
  to record support/refutation per Claim, and keep prior versions queryable.
- #76 owns provider egress, ACL for any remote controls, and deletion across
  durable knowledge/indexes/backups. Enable provider or scheduler wiring
  only after those boundaries are enforced.
