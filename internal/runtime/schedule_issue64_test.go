package runtime

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"q-agent/internal/llm"
	"q-agent/internal/state"
)

func TestIssue64ListLiveOnlyAndHint(t *testing.T) {
	r, _, _ := fixture(t)
	owner7 := input(4, -10, 7, 1, "create")
	owner0 := input(5, -10, 0, 1, "other")
	tools7 := &scheduleTools{base: noTools{}, runtime: r, owner: owner7}
	at := time.Now().Add(time.Hour).Format(time.RFC3339)
	// Done one-shot should not appear in list (live-only view).
	onceArgs, _ := json.Marshal(map[string]string{"kind": "reminder", "text": "once task live check", "at": at, "repeat": "once", "timezone": "Asia/Taipei"})
	out, err := tools7.Execute(context.Background(), llm.ToolCall{Name: "schedule_create", Arguments: onceArgs})
	if err != nil {
		t.Fatal(err)
	}
	var created map[string]any
	if err := json.Unmarshal([]byte(out), &created); err != nil {
		t.Fatal(err)
	}
	id, _ := created["id"].(string)
	// Fire it via ScanDue past due: need to advance time. Instead directly
	// check ledger exclusion by deleting? Simpler: fire via Store.ScanDue with
	// future time far enough that NextRunAt zeroes.
	// The once fires at `at`; scanning after at+minute marks done.
	fireAt, _ := time.Parse(time.RFC3339, at)
	if err := r.Store.ScanDue(fireAt.Add(2 * time.Minute)); err != nil {
		t.Fatal(err)
	}
	listOut, err := tools7.Execute(context.Background(), llm.ToolCall{Name: "schedule_list", Arguments: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	var list map[string]any
	if err := json.Unmarshal([]byte(listOut), &list); err != nil {
		t.Fatal(err)
	}
	if total, _ := list["total"].(float64); total != 0 {
		t.Fatalf("done once must be excluded from live list, got %s", listOut)
	}
	if _, ok := list["live_total"]; !ok {
		t.Fatalf("live_total missing: %s", listOut)
	}
	// Done ID must not appear in live schedules (recent_runs may still reference it).
	schedJSON, _ := json.Marshal(list["schedules"])
	if strings.Contains(string(schedJSON), id) {
		t.Fatalf("done ID should not appear in live schedules: %s", listOut)
	}
	// Create live daily in topic 7, check wrong-topic hint.
	at2 := time.Now().Add(2 * time.Hour).Format(time.RFC3339)
	dailyArgs, _ := json.Marshal(map[string]string{"kind": "agent", "text": "live daily hint check", "at": at2, "repeat": "daily", "timezone": "Asia/Taipei"})
	if _, err := tools7.Execute(context.Background(), llm.ToolCall{Name: "schedule_create", Arguments: dailyArgs}); err != nil {
		t.Fatal(err)
	}
	tools0 := &scheduleTools{base: noTools{}, runtime: r, owner: owner0}
	list0, err := tools0.Execute(context.Background(), llm.ToolCall{Name: "schedule_list", Arguments: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(list0, "other_topic_count") || !strings.Contains(list0, "7") {
		t.Fatalf("hint missing: %s", list0)
	}
	_ = id
}

func TestIssue64DeleteUnionNotFoundAndNote(t *testing.T) {
	r, _, _ := fixture(t)
	owner := input(4, -10, 7, 1, "x")
	tools := &scheduleTools{base: noTools{}, runtime: r, owner: owner}
	at := time.Now().Add(time.Hour).Format(time.RFC3339)
	at2 := time.Now().Add(61 * 60 * time.Second).Format(time.RFC3339)
	a1, _ := json.Marshal(map[string]string{"kind": "agent", "text": "union alpha daily", "at": at, "repeat": "daily", "timezone": "Asia/Taipei"})
	o1, err := tools.Execute(context.Background(), llm.ToolCall{Name: "schedule_create", Arguments: a1})
	if err != nil {
		t.Fatal(err)
	}
	var c1 map[string]any
	json.Unmarshal([]byte(o1), &c1)
	id1, _ := c1["id"].(string)
	a2, _ := json.Marshal(map[string]string{"kind": "agent", "text": "union alpha daily!", "at": at2, "repeat": "daily", "timezone": "Asia/Taipei"})
	if _, err := tools.Execute(context.Background(), llm.ToolCall{Name: "schedule_create", Arguments: a2}); err != nil {
		t.Fatal(err)
	}
	// Union ids + text: explicit id1 plus fuzzy text matching both.
	delArgs, _ := json.Marshal(map[string]any{"ids": []string{id1, "bogus-id"}, "text": "union alpha daily"})
	out, err := tools.Execute(context.Background(), llm.ToolCall{Name: "schedule_delete", Arguments: delArgs})
	if err != nil {
		t.Fatal(err)
	}
	var res map[string]any
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatal(err)
	}
	if n, _ := res["deleted_count"].(float64); n != 2 {
		t.Fatalf("union should delete 2, got %s", out)
	}
	if nf, _ := res["not_found_ids"].([]any); len(nf) != 1 {
		t.Fatalf("not_found missing bogus, got %s", out)
	}
	if lr, _ := res["live_remaining"].(float64); lr != 0 {
		t.Fatalf("live_remaining should be 0, got %s", out)
	}
	note, _ := res["note"].(string)
	if !strings.Contains(note, "0 live remain") || !strings.Contains(note, "now empty") {
		t.Fatalf("conditional empty note missing: %s", out)
	}
	// Partial delete where one remains: note must not claim empty.
	b1, _ := json.Marshal(map[string]string{"kind": "agent", "text": "keep me daily one", "at": time.Now().Add(3 * time.Hour).Format(time.RFC3339), "repeat": "daily", "timezone": "Asia/Taipei"})
	oKeep, err := tools.Execute(context.Background(), llm.ToolCall{Name: "schedule_create", Arguments: b1})
	if err != nil {
		t.Fatal(err)
	}
	var ck map[string]any
	json.Unmarshal([]byte(oKeep), &ck)
	keepID, _ := ck["id"].(string)
	b2, _ := json.Marshal(map[string]string{"kind": "agent", "text": "remove me daily two", "at": time.Now().Add(4 * time.Hour).Format(time.RFC3339), "repeat": "daily", "timezone": "Asia/Taipei"})
	oRem, err := tools.Execute(context.Background(), llm.ToolCall{Name: "schedule_create", Arguments: b2})
	if err != nil {
		t.Fatal(err)
	}
	var cr map[string]any
	json.Unmarshal([]byte(oRem), &cr)
	remID, _ := cr["id"].(string)
	delOne, _ := json.Marshal(map[string]string{"id": remID})
	out2, err := tools.Execute(context.Background(), llm.ToolCall{Name: "schedule_delete", Arguments: delOne})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out2, "1 live remain") || strings.Contains(out2, "now empty") {
		t.Fatalf("partial note must not claim empty: %s", out2)
	}
	_ = keepID
}

func TestIssue64CreateConfirmRecreate(t *testing.T) {
	r, _, _ := fixture(t)
	owner := input(4, -10, 7, 1, "x")
	tools := &scheduleTools{base: noTools{}, runtime: r, owner: owner}
	at := time.Now().Add(time.Hour).Format(time.RFC3339)
	args, _ := json.Marshal(map[string]string{"kind": "agent", "text": "confirm guard task", "at": at, "repeat": "daily", "timezone": "Asia/Taipei"})
	out, err := tools.Execute(context.Background(), llm.ToolCall{Name: "schedule_create", Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	var c map[string]any
	json.Unmarshal([]byte(out), &c)
	id, _ := c["id"].(string)
	del, _ := json.Marshal(map[string]string{"id": id})
	if _, err := tools.Execute(context.Background(), llm.ToolCall{Name: "schedule_delete", Arguments: del}); err != nil {
		t.Fatal(err)
	}
	// Immediate exact recreate blocked.
	at2 := time.Now().Add(2 * time.Hour).Format(time.RFC3339)
	reArgs, _ := json.Marshal(map[string]string{"kind": "agent", "text": "confirm guard task", "at": at2, "repeat": "daily", "timezone": "Asia/Taipei"})
	if _, err := tools.Execute(context.Background(), llm.ToolCall{Name: "schedule_create", Arguments: reArgs}); err == nil || !strings.Contains(err.Error(), "recently deleted") {
		t.Fatalf("expected tombstone block, got %v", err)
	}
	// With confirm_recreate:true and explicit request, succeeds.
	reConfirm, _ := json.Marshal(map[string]any{"kind": "agent", "text": "confirm guard task", "at": at2, "repeat": "daily", "timezone": "Asia/Taipei", "confirm_recreate": true})
	if _, err := tools.Execute(context.Background(), llm.ToolCall{Name: "schedule_create", Arguments: reConfirm}); err != nil {
		t.Fatalf("confirmed recreate should succeed, got %v", err)
	}
}

func TestIssue64LedgerInjectionPrefix(t *testing.T) {
	r, _, _ := fixture(t)
	owner7 := input(4, -10, 7, 1, "x")
	tools7 := &scheduleTools{base: noTools{}, runtime: r, owner: owner7}
	at := time.Now().Add(time.Hour).Format(time.RFC3339)
	args, _ := json.Marshal(map[string]string{"kind": "agent", "text": "ledger prefix task", "at": at, "repeat": "daily", "timezone": "Asia/Taipei"})
	out, err := tools7.Execute(context.Background(), llm.ToolCall{Name: "schedule_create", Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	var c map[string]any
	json.Unmarshal([]byte(out), &c)
	id, _ := c["id"].(string)
	msgs := r.memoryHistoryWithQuery(owner7, state.Conversation{}, "ledger prefix")
	if len(msgs) == 0 || msgs[0].Role != "system" || !strings.Contains(msgs[0].Content, id) || !strings.Contains(msgs[0].Content, "Live schedules") {
		t.Fatalf("ledger must be first system block with ID, got %+v", msgs)
	}
	prompt := tools7.SystemPrompt()
	if !strings.Contains(prompt, "Only claim a schedule was stopped") || !strings.Contains(prompt, "confirm_recreate") {
		t.Fatal("system prompt missing stop/confirm rules")
	}
	if got := schedulePreview("a\n- forged | line"); strings.Contains(got, "\n") {
		t.Fatalf("preview must strip newlines, got %q", got)
	}
	if got := truncateLedgerBlock(strings.Repeat("x", 5000), 4000); len(got) > 4100 || !strings.Contains(got, "truncated") {
		t.Fatalf("truncate helper broken, len=%d", len(got))
	}
}

func TestIssue64TextOnlyNoMatchKeepsSiblingHint(t *testing.T) {
	r, _, _ := fixture(t)
	owner7 := input(4, -10, 7, 1, "create")
	owner0 := input(5, -10, 0, 1, "other")
	tools7 := &scheduleTools{base: noTools{}, runtime: r, owner: owner7}
	at := time.Now().Add(time.Hour).Format(time.RFC3339)
	args, _ := json.Marshal(map[string]string{"kind": "agent", "text": "sibling hint text task", "at": at, "repeat": "daily", "timezone": "Asia/Taipei"})
	if _, err := tools7.Execute(context.Background(), llm.ToolCall{Name: "schedule_create", Arguments: args}); err != nil {
		t.Fatal(err)
	}
	tools0 := &scheduleTools{base: noTools{}, runtime: r, owner: owner0}
	delText, _ := json.Marshal(map[string]string{"text": "sibling hint text task"})
	_, err := tools0.Execute(context.Background(), llm.ToolCall{Name: "schedule_delete", Arguments: delText})
	if err == nil || !strings.Contains(err.Error(), "other topic") {
		t.Fatalf("text-only no-match must keep sibling hint, got %v", err)
	}
}
