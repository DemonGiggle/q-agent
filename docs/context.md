# q-agent Context Composition

In what **order** everything is assembled into the model request.
Order matters: stable beginnings cache well, changing tails do not.

Companion doc: [Memory](memory.md) — how discussions are stored,
summarized, and picked back out. Related guides:
[Configuration & operations](operations.md) · [Console guide](console.md).

Memory notes are one input to this order, not the order itself.
This doc shows where the live ledger + picked notes land ([1..2]), where past
turns land ([4..7]), and what must stay changeable at the end ([8..12]).

## 0. Words we use here (read this first if memory doc was skipped)

- **Rulebook:** the fixed instructions that say who the bot is, what tools
  exist, and what is never allowed. Most stable part, sent first.
- **Memory notes:** the ranked long-term notes plus the 2 newest raw
  journal lines for this chat. Changes when summaries run or when you
  pin / clear notes.
- **Check-in reference:** the last companion hello's, only in topic 0,
  only if one was sent in the last 7 days.
- **Past turns:** previous questions + final replies of this topic,
  replayed oldest first. See
  [turn definition](memory.md#0-words-we-use-in-these-two-docs-read-this-first).
- **Current message:** what you just sent, with photos if any. New every
  time by design.
- **Tool work:** the model's intermediate calls and their results while
  your current message is being handled.
- **Follow-ups:** messages you send while the bot is still working. They
  join the running job, they never undo a tool that already ran.
- **Recovery note:** a one-time hint added to this request only, after the
  model repeats the exact same tool call twice.
- **Toolbox:** the list of tools the model may call, with names and
  parameters. Sent alongside messages. Its order must stay fixed or the
  cache breaks even when words are identical.

## 1. What the model actually sees

This is the order that matters for prompt caching. Stable content goes
first, changeable content goes last.

### 1.1 Task request (doing your job)

```text
Messages sent to the model       Toolbox sent alongside
============================     ======================

[0] rulebook
    who the bot is, safety rules
    STABLE across turns

+--------------------------------+
| past plus current question     |
+--------------------------------+
[1] live schedule ledger (if any)
    "Live schedules in this topic (N): ..."
    + sibling-topic hint + recently-deleted guard
    CHANGES when: schedule create/delete in this chat
    OMITTED when: no live, no siblings, no recent deletes
    (same omission rule as check-in reference)

[2] memory notes
    "Long-term memory ..." +
    "Unconsolidated ..."
    CHANGES when: nightly summary runs,
    you pin / clear, journal grows

[3] check-in reference (if any)
    "Previous companion ..."
    only in topic 0,
    only if a hello was sent in 7 days
    last 3 at most

[4] past turn 1, your question
[5] follow-ups attached to it
[6] interruption note (if last run failed)
    "Task was interrupted ..."
    only if last run in this topic failed
[7] past turn 1, bot's final reply
 ... repeat per turn, oldest first ...

[8] current message  <-- what you just sent
    text (+ photos)
    NEW every time
+--------------------------------+
| work added while handling [8]  |
+--------------------------------+
[9] model's tool calls, step by step
[10] tool results per call, labeled reference-only
[11] follow-ups arriving mid-task,
     merged in, never undo side effects
[12] recovery hint, only after 2 identical
     tool calls, this request only
```

Key properties:

- [0] is the cache anchor. Changing model, way of thinking, or the
  toolbox breaks reuse from here on.
- Ledger [1] and memory [2] sit **before** history, not after. When notes
  change, every line after shifts position. Keep both small (ledger ~4 KB
  hard cap: 20 live + 20 recent lines, memory 6 KB default) and expect one
  cache miss on summary nights or schedule create/delete. Empty ledger is
  omitted like the check-in reference, so empty topics share one prefix
  shape.
- History [4..7] only grows at the end within its cap (12 turns / 32 KB,
  oldest dropped first). Old turns are never rewritten in place.
- The end [8..12] is the most changeable: new question, tool results,
  follow-ups, recovery hints. Cache prefixes that include the end hit
  less often; prefixes that stop before it hit more often.

### 1.2 Where tools fit

Tools ride alongside messages, not inside them. Their names, order, and
parameter descriptions are part of the cache key on providers that
support prompt caching: renaming a tool, reordering the list, or
rewording a parameter breaks reuse even when messages are identical.
Keep the toolbox order fixed and change it only together with the
rulebook.

### 1.3 Cache guidance (prefix caching)

Prefix caching reuses the longest stable beginning. The order above is
designed for it:

```text
 GOOD (stable -> changeable)       BAD (changeable first)

 [rulebook]                        [current time / random id]
 [toolbox, fixed order]            [rulebook]
 [memory, small, changes daily]    [history shuffled]
 [old history, only grows]         [memory re-sorted per question]
 [new question + tool results]     [toolbox in random order]
 ^^^^^^^^^^^^^^^^^^^^^^^^
 cache this beginning, recompute the end
```

Practical rules used by q-agent:

1. Pin the rulebook and toolbox order. Do not put per-request timestamps,
   request IDs, or shuffled keys ahead of history.
2. Keep memory small and labeled. A 6 KB picked slice re-caches faster
   than the full 32 KB note store.
3. Treat raw journal + current message + tool outputs as the changeable
   end. They change every turn by design; do not try to cache past them.
4. Expect a nightly miss after the summary lands (memory [2] changed) and
   after pin / clear / compress / trim. This is normal, not a bug.
5. For the check-in decider (no tools, more creative, 6 KB cap), freshness
   matters more than cache: it re-reads history + memory + live chatter
   every sweep by design.

Ideas if you want higher hit rates:

- Split memory into two notes: rarely-changed pinned facts first, then
  often-changed ranked + raw journal, so the first half keeps hitting
  after the second half churns.
- Move the check-in reference [3] behind history, or drop it when empty
  (already dropped when empty), so empty topics share one prefix shape.
- Record toolbox order at startup and log it; a changing order explains
  a sudden miss storm.
- Graph cached vs uncached tokens per step after enabling the provider's
  caching switch.

## 2. Companion and proactive context (separate path)

The companion sweep does **not** reuse the task request. It is read-only,
uses no tools, and is capped:

```text
check-in decider input, up to 6 KB:
  rulebook: how to judge, warmth-by-default, reply in fixed shape
  background + history:
    current time
    last chat activity (no per-message timestamps)
    recent companion hello's, up to 3, so it does not repeat
    past turns and memory, labeled reference-only:
      last 6 turns per topic ("You: ... / Bot: ...")
      picked memory notes ("Note [date]: ...")
      live group chatter inside the window, oldest first:
        one line each, capped length, with topic + sender
```

Empty history + memory + chatter means no model call at all. Group taps
cover chatter without a mention (private chats are never tapped; their
history is already complete). Taps expire after the window, are consumed
up to the sweep mark after a fully judged sweep, and are capped per chat.

## 3. Companion budgets cheat sheet

```text
check-ins ............ one chat, topic 0 only
  /companion ... ..... on|off|default|status|history, checks need no model
```

```text
[proactive] recent_window=12h ..... overheard window (max 168h)
[proactive] recent_max_*=200/32KiB  tap caps, oldest dropped first
[proactive] active_time=09:00-22:00 sweep window, randomized inside
[proactive] max_per_day=1 ......... rolling 24h cap, cooldown=6h
```

Memory budgets live in the
[memory](memory.md#5-commands-and-budgets-cheat-sheet) doc, next to the
stores they bound.

## 4. Failure and follow-up tails (summary)

Full save and cleaning rules live in
[memory](memory.md#6-failure-and-privacy-notes). What matters for order:

- Failed tasks save one bounded interruption note (up to 16 KB) offered
  to the next follow-up in the **same topic** as reference, never as a
  replay queue. It lands in history [4..7], oldest first.
- Follow-ups merge up to 16 messages / 64 KB into the next request at the
  end [11]. Other topics or users cannot inject. Unsent follow-ups live
  only in memory; a restart drops them.
- Safety labeling: tool results, check-in context, interruption notes,
  and web pages are wrapped as reference-only so content cannot pretend
  to be the surrounding instructions.
