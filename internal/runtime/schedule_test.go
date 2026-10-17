package runtime

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"q-agent/internal/llm"
	"q-agent/internal/platform"
	"q-agent/internal/state"
)

func enqueue(t *testing.T, r *Runtime, m platform.Message, kind string, at time.Time) state.Schedule {
	t.Helper()
	v, err := r.Store.CreateSchedule(state.Schedule{Route: m, Kind: kind, Text: "scheduled work", At: at, Repeat: "once", Timezone: "Asia/Taipei"}, at.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err = r.Store.ScanDue(at); err != nil {
		t.Fatal(err)
	}
	return v
}
func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestScheduledReminderBypassesModelAndPreservesTopic(t *testing.T) {
	r, w, p := fixture(t)
	m := input(0, 10, 7, 1, "")
	enqueue(t, r, m, "reminder", time.Now())
	must(t, r.dispatchScheduled(context.Background()))
	r.Wait()
	if w.calls != 0 || len(p.requests) != 0 {
		t.Fatal("reminder ran agent")
	}
	reply, ok := r.Store.Pending()
	if !ok || reply.Route.ChatID != 10 || reply.Route.TopicID != 7 || reply.Parts[0] != "scheduled work" {
		t.Fatalf("%+v", reply)
	}
	must(t, r.dispatchScheduled(context.Background()))
	must(t, r.Store.Delivered(reply.ID))
	if _, ok := r.Store.Pending(); ok {
		t.Fatal("duplicate reply")
	}
}
func TestScheduledTasksWaitForBusyChatAndGlobalCapacity(t *testing.T) {
	r, w, p := fixture(t)
	w.block = make(chan struct{})
	w.started = make(chan struct{}, 4)
	ctx := context.Background()
	must(t, r.Handle(ctx, input(1, 10, 7, 1, "interactive")))
	<-w.started
	must(t, r.Handle(ctx, input(2, 20, 0, 1, "interactive")))
	<-w.started
	now := time.Now()
	enqueue(t, r, input(0, 10, 8, 1, ""), "agent", now)
	enqueue(t, r, input(0, 30, 0, 1, ""), "agent", now)
	must(t, r.dispatchScheduled(ctx))
	if len(r.Store.PendingRuns()) != 2 {
		t.Fatal("busy jobs dropped")
	}
	close(w.block)
	r.Wait()
	// Pending jobs retain their eligibility even if they waited past grace.
	must(t, r.Store.ScanDue(now.Add(time.Hour)))
	must(t, r.dispatchScheduled(ctx))
	r.Wait()
	if len(r.Store.PendingRuns()) != 0 || len(p.requests) != 4 {
		t.Fatalf("pending=%d calls=%d", len(r.Store.PendingRuns()), len(p.requests))
	}
	if !strings.Contains(p.requests[2].Messages[len(p.requests[2].Messages)-1].Content, "scheduled work") {
		t.Fatal("saved instruction missing")
	}
	// No Telegram update IDs were consumed by scheduled work.
	if r.Store.Cursor() != 2 {
		t.Fatal("scheduler corrupted telegram cursor")
	}
	must(t, r.dispatchScheduled(ctx))
	r.Wait()
	if len(p.requests) != 4 {
		t.Fatal("completed jobs replayed")
	}
}
func TestScheduledAuthorizationRevokedAndDeleted(t *testing.T) {
	r, w, _ := fixture(t)
	owner := input(0, 10, 7, 1, "")
	at := time.Now()
	v := enqueue(t, r, owner, "agent", at)
	must(t, r.Store.DeleteSchedule(owner, v.ID))
	enqueue(t, r, owner, "reminder", at.Add(time.Second))
	r.Config.Telegram.ApprovedUsers = nil
	must(t, r.dispatchScheduled(context.Background()))
	r.Wait()
	if w.calls != 0 {
		t.Fatal("unauthorized task ran")
	}
	if _, ok := r.Store.Pending(); ok {
		t.Fatal("unauthorized notification sent")
	}
	if len(r.Store.PendingRuns()) != 0 {
		t.Fatal("revoked task retained")
	}
}
func TestSchedulerStartupDrainsPersistedPendingAndStops(t *testing.T) {
	r, _, p := fixture(t)
	p.started = make(chan struct{}, 1)
	enqueue(t, r, input(0, 10, 0, 1, ""), "agent", time.Now())
	must(t, r.StartScheduler(context.Background()))
	select {
	case <-p.started:
	case <-time.After(2 * time.Second):
		t.Fatal("startup did not drain queue")
	}
	r.Close()
	if err := r.StartScheduler(context.Background()); err == nil {
		t.Fatal("scheduler started after close")
	}
}
func TestSchedulerWakeDrainsAfterInteractiveCompletion(t *testing.T) {
	r, w, _ := fixture(t)
	w.block = make(chan struct{})
	w.started = make(chan struct{}, 4)
	ctx := context.Background()
	must(t, r.Handle(ctx, input(1, 10, 0, 1, "interactive")))
	<-w.started
	enqueue(t, r, input(0, 10, 0, 1, ""), "agent", time.Now())
	must(t, r.StartScheduler(ctx))
	close(w.block)
	// Both tasks reach the provider without waiting for a minute boundary.
	select {
	case <-w.started:
	case <-time.After(2 * time.Second):
		t.Fatal("queue not woken")
	}
	r.Close()
}
func TestScheduleToolsCreateListDeleteScopeAndSystemPrompt(t *testing.T) {
	r, _, _ := fixture(t)
	owner := input(4, 10, 7, 1, "create")
	tools := &scheduleTools{base: noTools{}, runtime: r, owner: owner}
	prompt := tools.SystemPrompt()
	if !strings.Contains(prompt, "Current time:") || !strings.Contains(prompt, "schedule_create") {
		t.Fatal("missing scheduling guidance")
	}
	if len(tools.Schemas()) != 3 {
		t.Fatal("missing schemas")
	}
	args, _ := json.Marshal(map[string]string{"kind": "reminder", "text": "keyboard", "at": time.Now().Add(time.Hour).Format(time.RFC3339), "repeat": "daily", "timezone": "Asia/Taipei"})
	output, err := tools.Execute(context.Background(), llm.ToolCall{Name: "schedule_create", Arguments: args})
	must(t, err)
	var created state.Schedule
	must(t, json.Unmarshal([]byte(output), &created))
	if created.Route.UserID != 1 || created.Route.TopicID != 7 || created.Route.UpdateID != 0 {
		t.Fatalf("%+v", created)
	}
	other := &scheduleTools{base: noTools{}, runtime: r, owner: input(0, 10, 8, 1, "")}
	deletion, _ := json.Marshal(map[string]string{"id": created.ID})
	if _, err = other.Execute(context.Background(), llm.ToolCall{Name: "schedule_delete", Arguments: deletion}); err == nil {
		t.Fatal("cross-topic deletion allowed")
	}
	list, err := tools.Execute(context.Background(), llm.ToolCall{Name: "schedule_list", Arguments: json.RawMessage(`{}`)})
	must(t, err)
	if !strings.Contains(list, created.ID) {
		t.Fatal("schedule missing from list")
	}
	_, err = tools.Execute(context.Background(), llm.ToolCall{Name: "schedule_delete", Arguments: deletion})
	must(t, err)
	schedules, _ := r.Store.ListSchedules(owner)
	if len(schedules) != 0 {
		t.Fatal("not deleted")
	}
	for _, raw := range []string{`{"id":"x","chat_id":20}`, `{} {}`, `null`} {
		_, err = tools.Execute(context.Background(), llm.ToolCall{Name: "schedule_list", Arguments: json.RawMessage(raw)})
		if err == nil {
			t.Fatalf("invalid arguments accepted: %s", raw)
		}
	}
}

func TestScheduleToolPaginationFitsOutputLimit(t *testing.T) {
	r, _, _ := fixture(t)
	owner := input(0, 10, 7, 1, "")
	at := time.Now().Add(time.Hour)
	for i := 0; i < 12; i++ {
		_, err := r.Store.CreateSchedule(state.Schedule{Route: owner, Kind: "reminder", Text: strings.Repeat("字", 2500), At: at.Add(time.Duration(i) * time.Minute), Repeat: "once", Timezone: "Asia/Taipei"}, time.Now())
		must(t, err)
	}
	tools := &scheduleTools{base: noTools{}, runtime: r, owner: owner}
	out, err := tools.Execute(context.Background(), llm.ToolCall{Name: "schedule_list", Arguments: json.RawMessage(`{}`)})
	must(t, err)
	if len(out) > 8192 {
		t.Fatalf("list exceeds tool output bound: %d", len(out))
	}
	var page struct {
		Schedules []state.Schedule `json:"schedules"`
		Next      int              `json:"next_offset"`
	}
	must(t, json.Unmarshal([]byte(out), &page))
	if len(page.Schedules) != 10 || page.Next != 10 {
		t.Fatalf("%+v", page)
	}
	out, err = tools.Execute(context.Background(), llm.ToolCall{Name: "schedule_list", Arguments: json.RawMessage(`{"offset":10}`)})
	must(t, err)
	must(t, json.Unmarshal([]byte(out), &page))
	if len(page.Schedules) != 2 || page.Next != -1 {
		t.Fatalf("%+v", page)
	}
}

func TestScheduledAgentFailureRecordedWithoutRetry(t *testing.T) {
	r, w, _ := fixture(t)
	w.fail = true
	owner := input(0, 10, 0, 1, "")
	enqueue(t, r, owner, "agent", time.Now())
	must(t, r.dispatchScheduled(context.Background()))
	r.Wait()
	_, runs := r.Store.ListSchedules(owner)
	if len(runs) != 1 || runs[0].Status != "failed" || len(runs[0].Parts) == 0 {
		t.Fatalf("%+v", runs)
	}
	must(t, r.dispatchScheduled(context.Background()))
	r.Wait()
	if w.calls != 1 {
		t.Fatal("failed work automatically retried")
	}
}

func TestScheduledAgentCancelDoesNotRetry(t *testing.T) {
	r, _, p := fixture(t)
	p.block = true
	p.started = make(chan struct{}, 1)
	owner := input(0, 10, 7, 1, "")
	enqueue(t, r, owner, "agent", time.Now())
	must(t, r.dispatchScheduled(context.Background()))
	select {
	case <-p.started:
	case <-time.After(2 * time.Second):
		t.Fatal("scheduled agent did not start")
	}
	must(t, r.Handle(context.Background(), input(1, 10, 7, 1, "/cancel")))
	r.Wait()
	_, runs := r.Store.ListSchedules(owner)
	if len(runs) != 1 || runs[0].Status != "cancelled" {
		t.Fatalf("%+v", runs)
	}
	must(t, r.dispatchScheduled(context.Background()))
	r.Wait()
	if len(p.requests) != 1 {
		t.Fatal("cancelled work retried")
	}
}

type describedTools struct{ noTools }

func (describedTools) SystemPrompt() string { return "host-mode sentinel" }
func TestScheduleToolsPreserveExecutionModePromptAndLocalTools(t *testing.T) {
	r, _, _ := fixture(t)
	tools := &scheduleTools{base: describedTools{}, runtime: r, owner: input(0, 10, 0, 1, "")}
	if !strings.HasPrefix(tools.SystemPrompt(), "host-mode sentinel") {
		t.Fatal("wrapper lost execution-mode instructions")
	}
	if _, err := tools.Execute(context.Background(), llm.ToolCall{Name: "shell", Arguments: json.RawMessage(`{}`)}); err == nil || !strings.Contains(err.Error(), "unexpected tool") {
		t.Fatal("local call not delegated")
	}
}
