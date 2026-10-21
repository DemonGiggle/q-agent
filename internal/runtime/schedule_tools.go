package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"q-agent/internal/agent"
	"q-agent/internal/llm"
	"q-agent/internal/platform"
	"q-agent/internal/redaction"
	"q-agent/internal/state"
)

type scheduleTools struct {
	base    agent.Tools
	runtime *Runtime
	owner   platform.Message
}

func (t *scheduleTools) SystemPrompt() string {
	system := agent.System
	if described, ok := t.base.(interface{ SystemPrompt() string }); ok {
		system = described.SystemPrompt()
	}
	return system + fmt.Sprintf("\nCurrent time: %s. Use schedule_create/list/delete for persistent reminders and scheduled agent tasks, never background sleeps or OS cron. Schedule times must be explicit RFC3339 timestamps with UTC offset; use an IANA timezone for local daily/weekly recurrence. If the user's timezone cannot be inferred, ask. Choose reminder for a literal notification (no model call when due), agent for future work. Only claim a schedule was created after the tool succeeds. Schedule tools are host-managed in both execution modes and scoped to this user, chat, and topic. Deleting a schedule cancels pending occurrences but cannot stop an already running task; use /cancel for that. Scheduler scans every minute; normal scan lateness is up to 60 seconds plus queue delay. Missed occurrences have a five-minute grace window; interrupted agent tasks are not retried automatically.", time.Now().Format(time.RFC3339))
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
		llm.Tool{Name: "schedule_create", Description: "Create a persistent one-time, daily, or weekly reminder or agent task for the current user/chat/topic. at must be in the future. Daily/weekly use at's wall time in timezone. Returns the stored ID and next run time.", Parameters: object(map[string]any{
			"kind":     map[string]any{"type": "string", "enum": []string{"reminder", "agent"}},
			"text":     text("Literal reminder text, or the saved agent task instruction (max 8192 bytes)."),
			"at":       text("First occurrence, RFC3339 timestamp with explicit UTC offset."),
			"repeat":   map[string]any{"type": "string", "enum": []string{"once", "daily", "weekly"}},
			"timezone": text("IANA timezone, e.g. Asia/Taipei. Required even for one-time reminders."),
		}, "kind", "text", "at", "repeat", "timezone")},
		llm.Tool{Name: "schedule_list", Description: "List this user's schedules in the current topic, 10 per page, plus 5 recent runs. Text is a short preview. Use next_offset to fetch another page.", Parameters: object(map[string]any{"offset": map[string]any{"type": "integer", "minimum": 0, "description": "Schedule page offset (default 0)."}})},
		llm.Tool{Name: "schedule_delete", Description: "Delete one owned schedule in this topic and cancel its pending runs. Already running tasks and completed replies are not cancelled.", Parameters: object(map[string]any{"id": text("Schedule ID returned by schedule_create/list.")}, "id")},
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
			Kind     string `json:"kind"`
			Text     string `json:"text"`
			At       string `json:"at"`
			Repeat   string `json:"repeat"`
			Timezone string `json:"timezone"`
		}
		if err := decodeArguments(call.Arguments, &args); err != nil {
			return "", err
		}
		at, err := time.Parse(time.RFC3339Nano, args.At)
		if err != nil {
			return "", err
		}
		v, err := t.runtime.Store.CreateSchedule(state.Schedule{Route: t.owner, Kind: args.Kind, Text: args.Text, At: at, Repeat: args.Repeat, Timezone: args.Timezone}, time.Now())
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
		schedules, runs := t.runtime.Store.ListSchedules(t.owner)
		start := args.Offset
		if start > len(schedules) {
			start = len(schedules)
		}
		end := start + 10
		if end > len(schedules) {
			end = len(schedules)
		}
		page := append([]state.Schedule{}, schedules[start:end]...)
		for i := range page {
			page[i].Text = schedulePreview(page[i].Text)
		}
		if len(runs) > 5 {
			runs = runs[len(runs)-5:]
		}
		recent := []map[string]any{}
		for _, run := range runs {
			recent = append(recent, map[string]any{"id": run.ID, "schedule_id": run.ScheduleID, "scheduled_at": run.ScheduledAt, "status": run.Status, "delivered_chunks": run.Next, "total_chunks": len(run.Parts)})
		}
		nextOffset := -1
		if end < len(schedules) {
			nextOffset = end
		}
		result = map[string]any{"schedules": page, "total": len(schedules), "next_offset": nextOffset, "recent_runs": recent}
	case "schedule_delete":
		var args struct {
			ID string `json:"id"`
		}
		if err := decodeArguments(call.Arguments, &args); err != nil {
			return "", err
		}
		if err := t.runtime.Store.DeleteSchedule(t.owner, args.ID); err != nil {
			return "", err
		}
		result = map[string]string{"deleted": args.ID}
	}
	out, err := json.Marshal(result)
	return string(out), err
}

func schedulePreview(text string) string {
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

func (t *scheduleTools) TaskError() error {
	if scoped, ok := t.base.(interface{ TaskError() error }); ok {
		return scoped.TaskError()
	}
	return nil
}
