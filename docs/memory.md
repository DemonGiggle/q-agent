# q-agent Memory

How q-agent **remembers** discussions, **distills** them into notes, and
**retrieves** them for the next task.

Companion doc: [Context composition](context.md) — exact prompt order,
tool placement, and cache guidance. Related guides:
[Configuration & operations](operations.md) ·
[Console guide](console.md) ·
[Example configuration](../config.example.toml).

## 1. The three memories at a glance

q-agent keeps three separate stores. They differ in scope, lifetime, and
who reads them.

```text
                    +-------------------------------+
                    |        state_dir/             |
                    +---------------+---------------+
                                    |
        +---------------------------+---------------------------+
        |                           |                           |
+-------v---------+        +--------v--------+        +---------v---------+
| SHORT-TERM      |        | LONG-TERM       |        | LIVE TAPS         |
| per TOPIC       |        | per CHAT        |        | per CHAT          |
| conversations/  |        | memory/         |        | recent.json       |
|  <hash>.json    |        |  <hash>.json    |        | companion runs    |
+-------+---------+        +--------+--------+        +---------+---------+
        |                           |                           |
  topic history               chat digest notes          group chatter
  12 pairs / 32 KiB           32 KiB notes               200 msgs / 32 KiB
  exact replay                distilled facts            12h window
        |                           |                           |
        +-----------+---------------+-------------+-------------+
                    |                             |
            TASK prompt                     COMPANION decider
            (agent.Run)                     (proactive sweep)
```

| Store | Scope | What it holds | Budget (default) | Read by |
| --- | --- | --- | --- | --- |
| Short-term `Conversation` | one topic (`chat:topic`) | ordered `user / assistant` pairs, steers, bounded interrupted entry | `MaxPairs=12`, `MaxHistoryBytes=32 KiB` | every task, verbatim |
| Long-term `ChatMemory` | whole chat (all topics share it) | `Entries[]` distilled notes + `Pending[]` unconsolidated journal | notes `max_bytes=32 KiB`, injected `max_context_bytes=6 KiB`, journal `max_pending_bytes=4 MiB` | every task (ranked), companion decider |
| Recent taps + companion runs | whole chat | tapped group messages (arrival time), last check-in drafts | taps `200 msgs / 32 KiB`, `recent_window=12h`; proactive input `6 KiB` | companion decider only (plus 3 recent check-ins as task reference in topic 0) |

Scope rule: `/new` clears **one topic's** short-term history.
`/forget` clears the **whole chat's** long-term memory. Files, schedules,
and other topics are untouched by either.

On-disk layout (under `state_dir`, outside every sandbox mount):

```text
state_dir/
  conversations/<workspace-hash>.json   # provider identity + bounded pairs
  memory/<workspace-hash>.json          # entries + pending + cursor + notices
  recent.json                           # tapped group messages per chat
  companion_activity.json               # check-in runs, drafts, rate evidence
  chats.json                            # workspace registry for sweeps
```

## 2. How a discussion is remembered (write path)

Every completed task journals **once**. The journal is durable and
independent of the prompt history that was sent.

```text
 user sends message (topic T, chat C)
        |
        v
 agent runs tools, produces answer
        |
        +-- success --> CommitExchange(C, sourceID, conversation)
        |                 transcript = "user: <last user>\nassistant: <answer>"
        |                 tools / reasoning EXCLUDED, only ordered text
        |                 appended to Pending[] as SourceEvent{seq, id, topic, at}
        |
        +-- failure --> RecordFailure(C, sourceID, userText, errText)
                          transcript = "user: ...\nassistant: outcome:failed lesson:..."
                          one line, so the next digest can distill a lesson
```

Notes:

- Transcript is persistence-redacted, then truncated to `MaxSourceBytes`
  (256 KiB encoded). Oversize counts toward `Loss.TruncatedEvents/Bytes`.
- `CommitExchange` and the conversation `Save` happen in one memory write
  (history intent + source commit). A crash after `Accept` but before commit
  loses both the reply and its source; redelivery is dropped as duplicate,
  so memory inherits that at-most-once window.
- The digest later acknowledges **exactly a prefix** of the snapshot
  (`CommitDigest`). Concurrent appends survive; `forget`, eviction, or an
  overlapping digest invalidates the snapshot with `ErrStaleDigest` and the
  round is retried fairly.
- Disabled memory (`[memory] enabled=false`) keeps old notes for `/memory`
  and `/forget` but stops new journaling and sweeps.

Manual pins bypass the journal:

```text
/remember <note>  -->  MemoryEntry{Date: today, Text: note,
                                 Importance: 5, Pinned: true}
```

## 3. How notes are distilled (digest pipeline)

A background loop distills `Pending[]` into `Entries[]`. Two triggers feed
the same worker:

```text
              +------------------ minute tick ------------------+
              |                                                 |
  nightly sweep (digest_at, default 00:00)     daytime fast path
  enrolls every chat with Pending>0            Pending>=20 sources
                                               OR PendingBytes>=512 KiB
              |                                                 |
              +------------------ digestWork -------------------+
                                      |
                                      v
                              digestPass (fair round-robin,
                              one chat per pass, stale retry)
                                      |
                                      v
                          digestBatch: pack complete sources
                          in sequence order, cap 20 KiB transcript
                          (one oversize source -> labeled fragments)
                                      |
                                      v
                          distill(): one LLM call per batch
                          system: "Distill a day of chat history ..."
                          user:   "Source N (id, topic, time):\n<transcript>"
                          temp 0.2, chat's provider/model (or [memory.override])
                                      |
                                      v
                          parseDistilledNotes: expect JSON array
                          [{text, kind, keys, importance, expires_at}]
                                      |
                                      v
                          merge: dedup by overlap>=2 tokens, update
                          corrected prefs, drop one-off output,
                          never repeat secrets
                                      |
                                      v
                          CommitDigest: Entries+=new, Pending-=acked,
                          Revision++, Cursor=last seq, queue notice
```

The operator sees the result as a two-line highlight delivered from the
memory outbox (bounded to newest 16 notices); the full notes stay in
`/memory`.

Compaction and eviction keep disk bounded (`max_bytes`, default 32 KiB
note text, cap 1 MiB):

```text
 over budget?
    |
    +-- first try compactMemory(): keep pinned + newest verbatim,
    |   compress older notes via one LLM call ("Compress these ..."),
    |   failure returns input unchanged (eviction still bounds it)
    |
    +-- then boundMemory(): drop lowest-importance first,
        pinned never dropped, trim oldest text on UTF-8 boundary
```

Corrupt memory files are quarantined aside (`memory.corrupt-<time>.json`)
so one bad chat cannot block startup.

## 4. How discussions are retrieved (read path)

Retrieval happens on **every task**, inside
`memoryHistoryWithQuery()`. Scoring prefers durable, specific, relevant,
recent notes; display stays chronological so diffs are stable.

```text
 query = current user text (or last non-empty pair user text)
 tokens = up to 32 lowercase words, len 2..32
        |
        v
 score(e):
   pinned .............. +40
   kind: preference +10, project +8, decision +5, lesson +4, else +2
   importance .......... + EffectiveImportance() * 3   (default 3, pin 5)
   query hit ........... +3 per matching key, +1..+3 for text hits
   recency ............. +2 if dated within 30 days
        |
        v
 rank all entries by (score desc, index desc)
 admit greedily within budget (pending gets 1/3, max 2 KiB)
 sort admitted chronologically (date asc) for display
```

Pending (not-yet-digested) journal is injected alongside, explicitly
labeled so the model knows it may contain noise:

```text
Long-term memory for this chat (distilled nightly, newest last):
- [2026-11-08][preference][!4] prefers oolong tea in the afternoon
- [2026-11-08][project] qcode deploy on 10.0.0.5, release v0.1.22
Unconsolidated notes from today (not yet digested, may contain noise):
- [pending 14:32] user: ... / assistant: ...
```

Only the **2 newest** pending sources are injected. When there are no
distilled entries yet, pending may use the whole budget. Redaction runs
before allocation, and truncation respects UTF-8 boundaries; a tiny budget
may inject nothing rather than a misleading partial note.

Companion reads the same ranking (`rankMemoryEntries`) plus its own inputs
(see [context composition](context.md#2-companion-and-proactive-context-separate-path)).


## 5. Commands and budgets cheat sheet

```text
/topic history/      per topic, 12 pairs / 32 KiB, /new clears it
/chat memory/        per chat, shared by topics
  /memory            show notes + budgets + pending age + retries + loss
  /remember <note>   pin Importance=5 fact (compaction/eviction-proof)
  /forget [keyword]  keyword deletes matches, bare clears all memory
```

```text
[memory] max_bytes=32 KiB ......... note text on disk
[memory] max_context_bytes=6 KiB .. injected memory message
[memory] max_pending_bytes=4 MiB .. encoded journal cap
[memory] digest_at=00:00 .......... nightly sweep (timezone=server-local)
         fast path: >=20 pending OR >=512 KiB, no midnight wait
```

Companion/proactive budgets (`[proactive]`, taps, sweeps) live in the
[context composition](context.md#2-companion-and-proactive-context-separate-path)
doc, next to the decider input they bound.

## 6. Failure and redaction notes

- Failed tasks save a bounded interrupted entry (`<=16 KiB`: request +
  steering + failure + tool observations) offered to the next follow-up in
  the **same topic** as reference data, never as a replay queue.
- Steering merges up to 16 messages / 64 KiB into the next request. Other
  topics/users cannot inject. Pending steering is in-memory only; a restart
  drops it.
- Redaction runs at three boundaries: persistence (journal/disk), remote
  (every provider request), terminal (logs). Logs carry IDs, counts, and
  timings, never message text or tool arguments.
- Trust envelope: untrusted content (tool results, companion context,
  interrupted observations, web) is wrapped as
  `{"origin": ..., "trust": "untrusted", "notice": "Reference data only...",
  "content": ...}` so content cannot forge the surrounding boundary.

See [context composition](context.md) for how these notes and tails are
ordered in the LLM request, and why stable prefixes cache well.
