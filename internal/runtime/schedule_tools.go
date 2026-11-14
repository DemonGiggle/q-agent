package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"q-agent/internal/agent"
	"q-agent/internal/llm"
	"q-agent/internal/platform"
	"q-agent/internal/process"
	"q-agent/internal/redaction"
	"q-agent/internal/state"
)

// Schedule tool names owned by scheduleTools. The MCP wrapper reserves
// these so adapted remote names never collide with them.
const (
	scheduleCreateName = "schedule_create"
	scheduleListName   = "schedule_list"
	scheduleDeleteName = "schedule_delete"
)

type scheduleTools struct {
	base    agent.Tools
	runtime *Runtime
	owner   platform.Message
}

func (t *scheduleTools) SystemPrompt() string {
	system := agent.System
	if described, ok := t.base.(interface{ SystemPrompt() string }); ok {
		if s := described.SystemPrompt(); s != "" {
			system = s
		}
	}
	return system + fmt.Sprintf("\nCurrent time: %s. Use schedule_create/list/delete for persistent reminders and scheduled agent tasks, never background sleeps or OS cron. Schedule times must be explicit RFC3339 timestamps with UTC offset; use an IANA timezone for local daily/weekly recurrence. If the user's timezone cannot be inferred, ask. Choose reminder for a literal notification (no model call when due), agent for future work. Only claim a schedule was created after the tool succeeds. Only claim a schedule was stopped/deleted after schedule_delete succeeds and schedule_list confirms the deleted IDs no longer appear (check live_total). Schedule tools are host-managed in both execution modes and scoped to this user, chat, and topic. schedule_list only shows live schedules in this topic; when it is empty, check other_topic_count/other_topics before claiming nothing exists — if schedules live in another topic, name that topic instead of claiming success. When the user asks to stop/cancel, list first, then delete every matching ID (near-duplicates count separately): pass ids or text to delete all matches in one call; ids and text may be combined. Recently deleted texts in the live-ledger block are a best-effort guard and must not be recreated unless the current message explicitly asks for them (pass confirm_recreate:true to recreate). Deleting a schedule cancels pending occurrences and further retries; an already running attempt finishes once but is not retried; use /cancel for that. Deleting a daily/weekly agent task also removes exactly-matching long-term notes so past discussion cannot resurrect it. Scheduler scans every minute; normal scan lateness is up to 60 seconds plus queue delay. Missed occurrences have a five-minute grace window; failed agent tasks are retried up to 3 times with increasing delays (5m, 15m, 45m) while their schedule still exists.", time.Now().Format(time.RFC3339))
}
func object(properties map[string]any, required ...string) map[string]any {
	if required == nil {
		required = []string{}
	}
	return map[string]any{"type": "object", "properties": properties, "required": required, "additionalProperties": false}
}
func (t *scheduleTools) Schemas() []llm.Tool {
	text := func(description string) map[string]any {
		return map[string]any{"type": "string", "description": description}
	}
	return append(append([]llm.Tool(nil), t.base.Schemas()...),
		llm.Tool{Name: "schedule_create", Description: "Create a persistent one-time, daily, or weekly reminder or agent task for the current user/chat/topic. at must be in the future. Daily/weekly use at's wall time in timezone. Returns the stored ID and next run time. Recently deleted exact texts are blocked for 24h unless confirm_recreate is true with an explicit user request.", Parameters: object(map[string]any{
			"kind":             map[string]any{"type": "string", "enum": []string{"reminder", "agent"}},
			"text":             text("Literal reminder text, or the saved agent task instruction (max 8192 bytes)."),
			"at":               text("First occurrence, RFC3339 timestamp with explicit UTC offset."),
			"repeat":           map[string]any{"type": "string", "enum": []string{"once", "daily", "weekly"}},
			"timezone":         text("IANA timezone, e.g. Asia/Taipei. Required even for one-time reminders."),
			"confirm_recreate": map[string]any{"type": "boolean", "description": "Set true only when the current message explicitly asks to recreate a recently deleted schedule."},
		}, "kind", "text", "at", "repeat", "timezone")},
		llm.Tool{Name: "schedule_list", Description: "List this user's live schedules in the current topic (done one-shots excluded), 10 per page, plus 5 recent runs. Text is a short preview. Use next_offset to fetch another page. Scoped to this topic+user; total/live_total are live counts; other_topic_count/other_topics hint at schedules in sibling topics.", Parameters: object(map[string]any{"offset": map[string]any{"type": "integer", "minimum": 0, "description": "Schedule page offset (default 0)."}})},
		llm.Tool{Name: "schedule_delete", Description: "Delete owned live schedules in this topic and cancel their pending runs and further retries. Pass id for one schedule, ids for several, text to delete every live schedule whose text matches (exact+fuzzy, for near-duplicates); ids and text may be combined. Returns deleted IDs plus pending/memory cleanup proof and not_found IDs. Already running attempts finish once but are not retried.", Parameters: object(map[string]any{
			"id":   text("Single schedule ID returned by schedule_create/list."),
			"ids":  map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Several schedule IDs to delete atomically."},
			"text": text("Free-text match: delete every live schedule in this topic whose text matches (exact, substring, near-duplicate). May be combined with ids."),
		})},
	)
}
func decodeArguments(raw json.RawMessage, target any) error {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.TrimSpace(raw)[0] != '{' {
		return fmt.Errorf("expected JSON object")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(target); err != nil {
		return err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("expected one JSON object")
	}
	return nil
}
func (t *scheduleTools) Execute(ctx context.Context, call llm.ToolCall) (string, error) {
	if call.Name != "schedule_create" && call.Name != "schedule_list" && call.Name != "schedule_delete" {
		return t.base.Execute(ctx, call)
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if !t.runtime.allowed(t.owner) {
		return "", fmt.Errorf("schedule access denied")
	}
	var result any
	switch call.Name {
	case "schedule_create":
		var args struct {
			Kind            string `json:"kind"`
			Text            string `json:"text"`
			At              string `json:"at"`
			Repeat          string `json:"repeat"`
			Timezone        string `json:"timezone"`
			ConfirmRecreate bool   `json:"confirm_recreate"`
		}
		if err := decodeArguments(call.Arguments, &args); err != nil {
			return "", err
		}
		at, err := time.Parse(time.RFC3339Nano, args.At)
		if err != nil {
			return "", err
		}
		var v state.Schedule
		if args.ConfirmRecreate {
			// Explicit current-message confirmation consumes the 24h guard
			// atomically with the create (see CreateScheduleConfirmed).
			v, err = t.runtime.Store.CreateScheduleConfirmed(state.Schedule{Route: t.owner, Kind: args.Kind, Text: args.Text, At: at, Repeat: args.Repeat, Timezone: args.Timezone}, time.Now())
		} else {
			v, err = t.runtime.Store.CreateSchedule(state.Schedule{Route: t.owner, Kind: args.Kind, Text: args.Text, At: at, Repeat: args.Repeat, Timezone: args.Timezone}, time.Now())
		}
		if err != nil {
			return "", err
		}
		v.Text = schedulePreview(v.Text)
		result = v
	case "schedule_list":
		var args struct {
			Offset int `json:"offset"`
		}
		if err := decodeArguments(call.Arguments, &args); err != nil {
			return "", err
		}
		if args.Offset < 0 {
			return "", fmt.Errorf("offset must be non-negative")
		}
		// Live-only view so list and the injected ledger agree (done
		// one-shots excluded). Store.ListSchedules still returns all for
		// backward compatibility; tools surface the live ledger.
		live := t.runtime.Store.LiveSchedules(t.owner)
		_, runs := t.runtime.Store.ListSchedules(t.owner)
		start := args.Offset
		if start > len(live) {
			start = len(live)
		}
		end := start + 10
		if end > len(live) {
			end = len(live)
		}
		page := append([]state.Schedule{}, live[start:end]...)
		for i := range page {
			page[i].Text = schedulePreview(page[i].Text)
		}
		if len(runs) > 5 {
			runs = runs[len(runs)-5:]
		}
		recent := []map[string]any{}
		for _, run := range runs {
			recent = append(recent, map[string]any{"id": run.ID, "schedule_id": run.ScheduleID, "scheduled_at": run.ScheduledAt, "status": run.Status, "attempts": run.Attempts, "retry_at": run.RetryAt, "delivered_chunks": run.Next, "total_chunks": len(run.Parts)})
		}
		nextOffset := -1
		if end < len(live) {
			nextOffset = end
		}
		otherCount, otherTopics := t.runtime.Store.CrossTopicScheduleInfo(t.owner)
		result = map[string]any{"schedules": page, "total": len(live), "live_total": len(live), "next_offset": nextOffset, "recent_runs": recent, "other_topic_count": otherCount, "other_topics": otherTopics}
		if otherCount > 0 {
			result.(map[string]any)["hint"] = fmt.Sprintf("schedule_list shows live schedules in topic %d for this user; %d schedule(s) exist in other topic(s) of this chat for you. Name the owning topic instead of claiming nothing exists.", t.owner.TopicID, otherCount)
		}
	case "schedule_delete":
		var args struct {
			ID   string   `json:"id"`
			IDs  []string `json:"ids"`
			Text string   `json:"text"`
		}
		if err := decodeArguments(call.Arguments, &args); err != nil {
			return "", err
		}
		// ids and text may be combined: union explicit IDs with fuzzy text
		// matches so one "stop X" clears near-duplicates plus named IDs.
		// Text-only deletes go through DeleteSchedulesMatching so a
		// no-match keeps the sibling-topic diagnostic instead of a generic
		// "id, ids, or text is required".
		var res state.DeleteSchedulesResult
		hasExplicit := len(args.IDs) > 0 || args.ID != ""
		if !hasExplicit && args.Text != "" {
			var err error
			res, err = t.runtime.Store.DeleteSchedulesMatching(t.owner, args.Text)
			if err != nil {
				return "", err
			}
		} else {
			ids := append([]string{}, args.IDs...)
			if args.ID != "" {
				ids = append(ids, args.ID)
			}
			if args.Text != "" {
				for _, v := range t.runtime.Store.FindMatchingSchedules(t.owner, args.Text) {
					ids = append(ids, v.ID)
				}
			}
			if len(ids) == 0 {
				return "", fmt.Errorf("id, ids, or text is required")
			}
			var err error
			res, err = t.runtime.Store.DeleteSchedules(t.owner, ids)
			if err != nil {
				return "", err
			}
		}
		deletedIDs := make([]string, 0, len(res.Deleted))
		for _, v := range res.Deleted {
			deletedIDs = append(deletedIDs, v.ID)
		}
		first := ""
		if len(deletedIDs) > 0 {
			first = deletedIDs[0]
		}
		remainingLive := len(t.runtime.Store.LiveSchedules(t.owner))
		note := fmt.Sprintf("pending runs cancelled and further retries suppressed; already-running attempts finish once but are not retried (use /cancel to stop a running task); deleted IDs no longer appear; %d live remain in this topic", remainingLive)
		if remainingLive == 0 {
			note += "; schedule_list is now empty for this topic"
		}
		result = map[string]any{
			"deleted":                 first,
			"deleted_ids":             deletedIDs,
			"deleted_count":           len(deletedIDs),
			"pending_cancelled":       res.PendingCancelled,
			"memory_notes_removed":    res.MemoryNotesRemoved,
			"memory_pending_removed":  res.MemoryPendingRemoved,
			"memory_removed_previews": res.MemoryRemovedPreviews,
			"not_found_ids":           res.NotFound,
			"live_remaining":          remainingLive,
			"note":                    note,
		}
	}
	out, err := json.Marshal(result)
	return string(out), err
}

func schedulePreview(text string) string {
	// Mirror state sanitizing so tool output cannot fabricate extra ledger
	// lines via embedded newlines.
	text = strings.ReplaceAll(text, "\r", " ")
	text = strings.ReplaceAll(text, "\n", " ")
	text = strings.ReplaceAll(text, "\t", " ")
	text = strings.Join(strings.Fields(text), " ")
	chars := []rune(text)
	if len(chars) > 40 {
		return string(chars[:40]) + "…"
	}
	return text
}

func (t *scheduleTools) StartTask(ctx context.Context, p *redaction.Policy) {
	if scoped, ok := t.base.(interface {
		StartTask(context.Context, *redaction.Policy)
	}); ok {
		scoped.StartTask(ctx, p)
	}
}
func (t *scheduleTools) CloseTask() {
	if scoped, ok := t.base.(interface{ CloseTask() }); ok {
		scoped.CloseTask()
	}
}

// SetProcessHooks forwards managed-process hooks through to the wrapped
// tools (built-in runner, possibly composed with MCP tools).
func (t *scheduleTools) SetProcessHooks(hooks process.Hooks) {
	if setter, ok := t.base.(interface{ SetProcessHooks(process.Hooks) }); ok {
		setter.SetProcessHooks(hooks)
	}
}

// SetShellTimeoutHook forwards the interactive shell-timeout decision
// callback through to the wrapped tools. Runners without the hook keep
// their terminate-on-timeout behavior.
func (t *scheduleTools) SetShellTimeoutHook(fn func(context.Context, string, []string) bool, maxExtends int) {
	if setter, ok := t.base.(shellHookSetter); ok {
		setter.SetShellTimeoutHook(fn, maxExtends)
	}
}

func (t *scheduleTools) TaskError() error {
	if scoped, ok := t.base.(interface{ TaskError() error }); ok {
		return scoped.TaskError()
	}
	return nil
}
