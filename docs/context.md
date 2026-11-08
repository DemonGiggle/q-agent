# q-agent Context Composition

In what **order** everything is assembled into the LLM request.
Order matters: stable prefixes cache well, volatile tails do not.

Companion doc: [Memory](memory.md) — storage layout, write/read paths,
digest pipeline, and retrieval scoring. Related guides:
[Configuration & operations](operations.md) ·
[Console guide](console.md).

Memory notes are an input to this order, not the order itself.
This doc shows where the ranked slice lands ([1]), where history lands
([3..6]), and what must stay volatile at the tail ([7..11]).

## 1. What the model actually sees

This is the order that matters for prompt caching. Stable content goes
first; volatile content goes last.

### 1.1 Task prompt (`agent.Run`)

```text
LLM request Messages[]          LLM request Tools[]
========================        ==================

[0] system                      tool schemas (6 locals + web +
      agent.System                shell/process/search/fetch...)
      or tools.SystemPrompt()     order must stay stable (see section 1.3)
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
  the tool set breaks the prefix (see section 1.3).
- Memory [1] sits **before** history, not after. When notes change, every
  message after [1] shifts position. Keep it small (`6 KiB` default) and
  expect a re-cache on digest nights.
- History [3..6] is append-only within the bound (`12 pairs / 32 KiB`,
  oldest dropped first). Edits never rewrite old pairs in place.
- The tail [7..11] is the most volatile: new user text, tool results,
  steers, recovery notices. Cache prefixes that include the tail hit less
  often; prefixes that stop before it hit more often.

### 1.2 Where tools fit

Tools ride alongside messages, not inside them. Their JSON schemas are
part of the cache key on providers that support prompt caching: renaming
a tool, reordering schemas, or changing a parameter description
invalidates the prefix even when messages are identical. Keep
`Schemas()` order deterministic and change it only with the system prompt.

### 1.3 Cache guidance (OpenAI / Anthropic prefix caching)

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


## 2. Companion and proactive context (separate path)

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


## 3. Companion budgets cheat sheet

```text
/companion/          per chat, topic 0 only
  /companion ...     on|off|default|status|history, diagnostics need no model
```

```text
[proactive] recent_window=12h ..... tap window (max 168h)
[proactive] recent_max_*=200/32KiB  tap caps, oldest evicted
[proactive] active_time=09:00-22:00 sweep window, randomized inside
[proactive] max_per_day=1 ......... rolling 24h cap, cooldown=6h
```

Memory budgets (`[memory]`, pairs, KiB caps, digest triggers) live in the
[memory](memory.md#5-commands-and-budgets-cheat-sheet) doc, next to the
stores they bound.

## 4. Failure and steering tails (summary)

Full persistence/redaction rules live in
[memory](memory.md#6-failure-and-redaction-notes). What matters for order:

- Failed tasks save a bounded interrupted entry (<=16 KiB) offered to the
  next follow-up in the **same topic** as reference data, never as a replay
  queue. It lands in history [3..6], oldest-first.
- Steering merges up to 16 messages / 64 KiB into the next request at the
  tail [10]. Other topics/users cannot inject. Pending steering is
  in-memory only; a restart drops it.
- Trust envelope: untrusted content (tool results, companion context,
  interrupted observations, web) is wrapped as
  `{"origin": ..., "trust": "untrusted", "notice": "Reference data only...",
  "content": ...}` so content cannot forge the surrounding boundary.
