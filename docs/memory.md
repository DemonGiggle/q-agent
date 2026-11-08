# q-agent Memory

How q-agent **remembers** discussions, **distills** them into notes, and
**retrieves** them for the next task.

Companion doc: [Context composition](context.md) — what order everything
is sent to the model in, where the toolbox sits, and how caching works.
Related guides: [Configuration & operations](operations.md) ·
[Console guide](console.md) · [Example configuration](../config.example.toml).

## 0. Words we use in these two docs (read this first)

These definitions avoid implementation names on purpose. Once you know
these five words, the rest reads plainly.

**Chat:** one Telegram conversation. A private DM with the bot is one chat.
One group is another chat.

**Topic:** one thread inside a chat. Telegram forum topics and message
threads map directly to this.

```text
What you see in Telegram            What q-agent keeps

Private DM with the bot       -->   one chat, always topic 0
Plain group, no threads       -->   one chat, everyone shares topic 0
Forum group with threads      -->   one chat, many topics:
  General                     -->     topic 1, its own short history
  Lunch, thread 7             -->     topic 7, its own short history
  Drama, thread 8             -->     topic 8, its own short history
  Message with no thread id   -->     topic 0, the main thread
```

File names on disk are hashes, so you cannot tell which file is which
chat from the name. Content is still fully separated per topic.

**Turn (one question plus one answer):** one message from you plus the one
final reply the bot sent back for it. This is the unit we count. It is
not one model call.

```text
you send one message
        |
        v
bot works for a while,
may call tools many times,
may talk to the model many times
        |
        v
bot sends one final reply in Telegram
        |
        v
saved as ONE turn:
  question = what you sent
  answer   = that final reply only
```

What is inside a turn, and what is not:

- Saved: your wording, the final reply wording.
- Not saved: tool outputs, web pages, reasoning steps, intermediate drafts.
- Follow-ups while the bot is still working do not open a new turn. They
  are attached to the same turn and only the final reply is kept.
- Commands like /new, /forget, /remember, /model never create a turn.
  They act immediately and leave history alone.

```text
Normal, two turns:

  Turn 0: you ask A / bot answers A
  Turn 1: you ask B / bot answers B

Follow-up while busy, still one turn:

  Turn 0: you ask A + you add "oh also..." / bot answers corrected A
          ^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^   ^^^^^^^^^^^^^^^^^^^^^^
          kept together                      only this version is kept
```

**Short-term history:** the last few turns of one topic, replayed word
for word. One topic has one short history.

**Long-term memory:** distilled notes for a whole chat, shared by all its
topics. Two parts:

- journal: raw day-to-day records, not yet summarized, may contain noise.
- distilled notes: short facts the nightly summarizer already cleaned up.

**Live taps:** recent group chatter the bot overheard without being
mentioned, kept 12 hours only. Used only to decide whether to check in,
never as task history. Private chats are never tapped.

## 1. The three memories at a glance

Same data folder, three jobs. They differ in scope, lifetime, and who
reads them.

```text
                    +-------------------------------+
                    |          data folder          |
                    +---------------+---------------+
                                    |
        +---------------------------+---------------------------+
        |                           |                           |
+-------v---------+        +--------v--------+        +---------v---------+
| SHORT-TERM      |        | LONG-TERM       |        | LIVE TAPS         |
| one per topic   |        | one per chat    |        | one per chat      |
| past turns      |        | journal + notes |        | overheard chatter |
+-------+---------+        +--------+--------+        +---------+---------+
        |                           |                           |
  last 12 turns               cleaned facts               last 200 msgs
  or 32 KB, exact             32 KB notes                 12 hour window
  replay                      shared by topics            group only
        |                           |                           |
        +-----------+---------------+-------------+-------------+
                    |                             |
              TASK request                  CHECK-IN decider
              (doing your job)              (should I say hi?)
```

| Store | Lives where | What it holds | Default limit | Who reads it |
| --- | --- | --- | --- | --- |
| Short-term | one topic | past questions + final answers in order, plus follow-ups and one bounded interruption note | last 12 turns, 32 KB total | every task, replayed exactly |
| Long-term | whole chat, shared by topics | distilled facts + not-yet-summarized journal | 32 KB notes on disk, 6 KB sent to model per task, 4 MB journal cap | every task (ranked) and check-in decider |
| Recent taps + check-in runs | whole chat | overheard group messages with arrival time, last check-in drafts | 200 msgs / 32 KB, 12 h window, 6 KB decider input | check-in decider only (plus last 3 check-ins shown as reference in topic 0) |

Scope rule: /new clears **one topic's** short history. /forget clears
the **whole chat's** long-term notes and journal. Files, schedules, and
other topics are untouched by either.

On disk, under the data folder and outside every sandbox mount:

```text
data-folder/
  conversations/<hash>.json   # which model + last turns of one topic
  memory/<hash>.json          # notes + journal + position + notices
  recent.json                 # overheard group messages per chat
  companion_activity.json     # check-in runs, drafts, rate proof
  chats.json                  # chat registry for background sweeps
```

Hash names are intentional: you cannot guess the chat from the file name.

## 2. How a discussion is remembered

Every finished task is journaled **once**. The journal is durable and
separate from the short history that is replayed next time.

```text
you send a message (in topic T, chat C)
        |
        v
bot works, uses tools, produces a final reply
        |
        +-- success --> journal one record:
        |                 "you: <your last message>
        |                  bot: <final reply>"
        |                 tool outputs excluded, only final wording
        |
        +-- failure --> journal one short lesson line:
                          "you: ...
                           bot: failed, lesson: ..."
                          so the next summary can learn from it
```

Notes:

- Records are cleaned for secrets, then cut if huge (256 KB encoded
  cap). Oversize counts toward a loss counter you can see in /memory.
- Short history save and journal append happen together. A crash between
  accepting your message and finishing the save can lose both the reply
  and its journal line; a redelivered duplicate is dropped, so memory
  inherits that at-most-once window.
- The later summarizer confirms exactly a prefix of what it saw.
  Records added during summarizing survive. If notes were cleared,
  trimmed, or another summary overlapped, that round is discarded and
  retried fairly.
- Turned-off memory keeps old notes visible for /memory and /forget but
  stops new journaling and background summaries.

Manual pins skip the journal entirely:

```text
/remember buy oat milk on Fridays  -->  pinned note, today, top importance
                                       never auto-deleted, never compressed away
```

## 3. How notes are distilled

A background loop turns the raw journal into short notes. Two triggers
feed the same worker:

```text
              +------------------ every minute ------------------+
              |                                                  |
  nightly sweep at midnight         daytime fast path when busy
  takes every chat with               20+ unsummarized records
  anything unsummarized               OR 512 KB of journal
              |                                                  |
              +------------------ same worker -------------------+
                                      |
                                      v
                              one chat at a time,
                              round-robin so no chat starves,
                              retry if notes changed underneath
                                      |
                                      v
                              pack records in order,
                              up to ~20 KB of text per batch
                              (one giant record is split
                               into labeled fragments)
                                      |
                                      v
                              one model call per batch,
                              low creativity, using this chat's
                              own model unless overridden:
                                "summarize a day of chat..."
                                + "Record N (topic, time): ..."
                                      |
                                      v
                              expect back a list of notes,
                              each with text, kind, search keys,
                              importance, optional expiry
                                      |
                                      v
                              merge: drop duplicates that share
                              2+ words, update a preference when
                              corrected, drop one-off outputs,
                              never repeat secrets
                                      |
                                      v
                              append new notes, drop confirmed
                              journal, advance position,
                              queue a two-line highlight
```

You see the result as a two-line highlight from the memory outbox (keeps
newest 16). Full notes stay in /memory.

Keeping disk bounded (default 32 KB of note text, hard cap 1 MB):

```text
over budget?
    |
    +-- first try to compress: keep pinned + newest word for word,
    |   ask the model to squeeze older notes smaller.
    |   If that fails, keep going — trimming below still bounds it.
    |
    +-- then trim: drop least important first,
        pinned are never dropped, cut oldest text on character boundary
```

A broken memory file is moved aside with a timestamp in the name so one
bad chat cannot block startup.

## 4. How discussions are retrieved

Retrieval happens on **every task**. Scoring prefers durable, specific,
relevant, recent notes. What is picked is ranked by score, what you see
is sorted by date so diffs stay stable.

The current question is the query. If it is empty, fall back to the last
non-empty question in history. It is split into up to 32 keywords.

Priority order, high to low:

- pinned notes always win by a large margin.
- kind: preference, then project, then decision, then lesson, then other.
- importance 1-5 (pins are 5, default is 3), weighted strongly.
- keyword match: match on search keys beats match on note text.
- dated within the last 30 days gets a small freshness bonus.

Then: rank everything by score, take what fits the 6 KB send budget
(journal gets at most one third, up to 2 KB), show taken notes oldest
first.

Not-yet-summarized journal is sent alongside, explicitly labeled so the
model knows it may contain noise:

```text
Long-term memory for this chat (distilled nightly, newest last):
- [2026-11-08][preference][!4] prefers oolong tea in the afternoon
- [2026-11-08][project] deploy on 10.0.0.5, release v0.1.22
Unconsolidated notes from today (not yet digested, may contain noise):
- [pending 14:32] you: ... / bot: ...
```

Only the **2 newest** journal records are sent. When no distilled notes
exist yet, journal may use the whole budget. Cleaning runs before
budgeting, cuts respect character boundaries, and a tiny budget may send
nothing rather than a misleading half note.

The check-in decider uses the same ranking plus its own inputs (see
[context composition](context.md#2-companion-and-proactive-context-separate-path)).

## 5. Commands and budgets cheat sheet

```text
short history ..... one topic, 12 turns / 32 KB, /new clears it
chat memory ....... whole chat, shared by topics
  /memory ......... show notes + budgets + journal age + retries + loss
  /remember <note>  pin one lasting fact, top importance, never auto-dropped
  /forget [word] .. with a word deletes matches only, bare clears all memory
```

```text
[memory] max_bytes=32 KiB ......... note text kept on disk
[memory] max_context_bytes=6 KiB .. notes sent to the model per task
[memory] max_pending_bytes=4 MiB .. raw journal cap
[memory] digest_at=00:00 .......... nightly summary (server local time)
         fast path: 20+ records OR 512+ KB, no midnight wait
```

Check-in budgets live in the
[context composition](context.md#2-companion-and-proactive-context-separate-path)
doc, next to the decider input they bound.

## 6. Failure and privacy notes

- Failed tasks save one bounded interruption note (up to 16 KB: request
  + follow-ups + failure + tool observations). It is offered to the next
  follow-up in the **same topic** as reference, never as a replay queue.
- Follow-ups merge up to 16 messages / 64 KB into the next request. Other
  topics or users cannot inject. Unsent follow-ups live only in memory;
  a restart drops them.
- Cleaning runs at three edges: when saving to disk, on every model
  request, and in logs. Logs carry IDs, counts, timings — never message
  text or tool arguments.
- Untrusted content (tool results, check-in context, interruption notes,
  web pages) is labeled as reference-only so it cannot pretend to be a
  system instruction or a chat command.

See [context composition](context.md) for how these notes and tails are
ordered in the model request, and why stable beginnings cache well.
