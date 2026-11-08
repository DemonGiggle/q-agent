# q-agent Memory & Context Composition

How q-agent **remembers** discussions, how it **retrieves** them, and in
what **order** everything is assembled into the LLM request. Order matters:
stable prefixes cache well, volatile tails do not.

Related guides: [Configuration & operations](operations.md) ·
[Console guide](console.md) · [Example configuration](../config.example.toml).

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
(see section 6).

## 5. What the model actually sees (context composition)

This is the order that matters for prompt caching. Stable content goes
first; volatile content goes last.

### 5.1 Task prompt (`agent.Run`)

```text
LLM request Messages[]          LLM request Tools[]
========================        ==================

[0] system                      tool schemas (6 locals + web +
      agent.System                shell/process/search/fetch...)
      or tools.SystemPrompt()     order must stay stable (see 5.3)
      STABLE across turns
                                  Temperature 0.2 (task/digest)
 +------------------------------+
 | history[] passed to agent.Run |
 +------------------------------+
[1] system
      "Long-term memory ..."    \
      + "Unconsolidated ..."     } memoryHistoryWithQuery()
      VOLATILE: changes on       /  re-ranked per query, grows
      digest / remember /        /   after every task
      forget
[2] user (untrusted)
      origin=companion_checkins  \
      "Previous companion ..."    } only in topic 0, only if a
      VOLATILE: last 3            /  check-in delivered in 7d
      delivered check-ins
[3] user                         \
      pair1.User                   |
[4] user (steers...)               | Conversation.Messages()
[5] user (untrusted)               | exact replay, oldest-first
      origin=interrupted_task_...  | interrupted entry only if
      "Task interrupted ..."       | last run failed (<=16 KiB)
[6] assistant                      |
      pair1.Answer                 |
 ... repeat per pair ...          /
[7] user  <-- current message
      m.Text (+ Images)
      VOLATILE: new every turn
 +------------------------------+
 | agent loop appends below      |
 +------------------------------+
[8] assistant (tool_calls) -----+ appended per model step
[9] tool (result per call) -----+ untrusted reference data
[10] user (steers arriving       } merged, never undoes side effects
     mid-task)
[11] system (recovery notice) --+ only after 2 identical tool calls,
                                 appended to THIS request only
```

Key properties:

- `system` [0] is the cache anchor. Changing provider/model/thinking or
  the tool set breaks the prefix (see 5.3).
- Memory [1] sits **before** history, not after. When notes change, every
  message after [1] shifts position. Keep it small (`6 KiB` default) and
  expect a re-cache on digest nights.
- History [3..6] is append-only within the bound (`12 pairs / 32 KiB`,
  oldest dropped first). Edits never rewrite old pairs in place.
- The tail [7..11] is the most volatile: new user text, tool results,
  steers, recovery notices. Cache prefixes that include the tail hit less
  often; prefixes that stop before it hit more often.

### 5.2 Where tools fit

Tools ride alongside messages, not inside them. Their JSON schemas are
part of the cache key on providers that support prompt caching: renaming
a tool, reordering schemas, or changing a parameter description
invalidates the prefix even when messages are identical. Keep
`Schemas()` order deterministic and change it only with the system prompt.

### 5.3 Cache guidance (OpenAI / Anthropic prefix caching)

Prefix caching reuses the longest stable prefix. The composition above
is designed for it:

```text
 GOOD (stable -> volatile)        BAD (volatile first)

 [system]                         [current time / random id]
 [tools (fixed order)]            [system]
 [memory (small, changes daily)]  [history shuffled]
 [old history (append-only)]      [memory re-sorted per query]
 [new user + tools results]       [tools in map order]
 ^^^^^^^^^^^^^^^^^^^^^^^^
 cache this prefix, recompute tail
```

Practical rules used by q-agent:

1. Pin `system` and tool order. Do not inject per-request timestamps,
   request IDs, or shuffled keys ahead of history.
2. Keep memory small and labeled. A `6 KiB` ranked slice re-caches faster
   than the full `32 KiB` note store.
3. Treat pending + current message + tool outputs as the volatile tail.
   They change every turn by design; do not try to cache past them.
4. Expect a nightly miss after the digest commits (memory [1] changed) and
   after `/remember` / `/forget` / compaction / eviction. This is normal.
5. For the companion decider (no tools, `temperature 0.7`, `6 KiB` cap),
   cache matters less than freshness: it re-reads history + memory + live
   taps every sweep by design.

Improvement ideas if you want higher hit rates:

- Split memory into two messages: `system: pinned/high-signal notes`
  (rarely changes) ahead of `system: query-ranked + pending` (changes
  often), so the first half keeps hitting after the second half churns.
- Move the companion reference [2] behind history, or drop it when empty
  (already done), so empty-topic prompts share one prefix shape.
- Hash tool schemas at startup and log the hash; a changing hash explains
  a sudden miss storm.
- Record `Usage.InputTokens / OutputTokens` per step (already in
  `Result.Usage`) and graph cached vs uncached tokens after enabling the
  provider's caching header.

## 6. Companion and proactive context (separate path)

The companion sweep does **not** reuse the task prompt. It is read-only,
tool-free, and capped:

```text
decideProactive():
  system: proactivePromptV1 (JSON verdict, warmth-by-default rules)
  user:   background + history, <= 6 KiB
            Current time: <RFC3339>
            Last chat activity: <RFC3339>   (no per-message timestamps)
            Recent companion check-ins (do not repeat, <=3)
            Conversation and memory (reference data only):
              last 6 pairs per topic ("User: ... / Assistant: ...")
              ranked memory notes ("Note [date]: ...")
              Live chat (last <window>, oldest first):
                [RFC3339 topic=X user=Y] <one line, <=500 runes>
```

Empty history + memory + taps means `skipped: empty history` without a
model call. Group taps cover chatter without a mention (private chats are
never tapped; their history is already complete). Taps expire after
`recent_window`, are consumed up to the sweep watermark after a terminal
evaluated sweep, and are per-chat capped.

## 7. Commands and budgets cheat sheet

```text
/topic history/      per topic, 12 pairs / 32 KiB, /new clears it
/chat memory/        per chat, shared by topics
  /memory            show notes + budgets + pending age + retries + loss
  /remember <note>   pin Importance=5 fact (compaction/eviction-proof)
  /forget [keyword]  keyword deletes matches, bare clears all memory
/companion/          per chat, topic 0 only
  /companion ...     on|off|default|status|history, diagnostics need no model
```

```text
[memory] max_bytes=32 KiB ......... note text on disk
[memory] max_context_bytes=6 KiB .. injected memory message
[memory] max_pending_bytes=4 MiB .. encoded journal cap
[memory] digest_at=00:00 .......... nightly sweep (timezone=server-local)
         fast path: >=20 pending OR >=512 KiB, no midnight wait
[proactive] recent_window=12h ..... tap window (max 168h)
[proactive] recent_max_*=200/32KiB  tap caps, oldest evicted
[proactive] active_time=09:00-22:00 sweep window, randomized inside
[proactive] max_per_day=1 ......... rolling 24h cap, cooldown=6h
```

## 8. Failure and redaction notes

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
