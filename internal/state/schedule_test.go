package state

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"q-agent/internal/platform"
)

func scheduleFixture(t *testing.T) (*Store, platform.Message, time.Time) {
	t.Helper()
	s, err := Open(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	return s, platform.Message{Platform: "telegram", ChatID: 10, TopicID: 7, UserID: 1}, time.Date(2026, 10, 17, 4, 30, 0, 0, time.UTC)
}
func addSchedule(t *testing.T, s *Store, owner platform.Message, at time.Time, kind, repeat string) Schedule {
	t.Helper()
	v, err := s.CreateSchedule(Schedule{Route: owner, Kind: kind, Text: "test task", At: at, Repeat: repeat, Timezone: "Asia/Taipei"}, at.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	return v
}
func check(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestScheduleScanDedupRestartAndDelivery(t *testing.T) {
	s, owner, at := scheduleFixture(t)
	v := addSchedule(t, s, owner, at, "reminder", "once")
	duplicate := addSchedule(t, s, owner, at, "reminder", "once")
	if duplicate.ID != v.ID {
		t.Fatal("duplicate creation")
	}
	check(t, s.ScanDue(at.Add(-time.Second)))
	if len(s.PendingRuns()) != 0 {
		t.Fatal("early run")
	}
	check(t, s.ScanDue(at.Add(time.Minute)))
	check(t, s.ScanDue(at.Add(2*time.Minute)))
	s, err := Open(s.dir, nil)
	check(t, err)
	check(t, s.ScanDue(at.Add(3*time.Minute)))
	pending := s.PendingRuns()
	if len(pending) != 1 || pending[0].ScheduleID != v.ID {
		t.Fatalf("%+v", pending)
	}
	// A queued run does not expire simply because its worker was busy.
	check(t, s.ScanDue(at.Add(time.Hour)))
	check(t, s.FinishRun(pending[0].ID, "succeeded", []string{"part1", "part2"}))
	reply, ok := s.Pending()
	if !ok || reply.Route.TopicID != 7 || reply.ID == "telegram:0" {
		t.Fatalf("%+v", reply)
	}
	check(t, s.Delivered(reply.ID))
	s, err = Open(s.dir, nil)
	check(t, err)
	reply, ok = s.Pending()
	if !ok || reply.Next != 1 {
		t.Fatal("delivery not resumed")
	}
	check(t, s.Delivered(reply.ID))
	check(t, s.FinishRun(reply.ID, "succeeded", []string{"duplicate"}))
	s, err = Open(s.dir, nil)
	check(t, err)
	check(t, s.RecoverRuns())
	check(t, s.ScanDue(at.Add(2*time.Hour)))
	if _, ok := s.Pending(); ok {
		t.Fatal("delivered output replayed")
	}
	if len(s.PendingRuns()) != 0 {
		t.Fatal("completed task replayed")
	}
}
func TestScheduleGraceAndCoalesce(t *testing.T) {
	for _, tc := range []struct {
		name, repeat string
		late         time.Duration
		status       string
		dueDelta     time.Duration
	}{
		{"within", "once", 5 * time.Minute, "pending", 0},
		{"expired", "once", 5*time.Minute + time.Nanosecond, "missed", 0},
		{"long daily downtime", "daily", 10*24*time.Hour + time.Minute, "pending", 10 * 24 * time.Hour},
		{"long weekly downtime", "weekly", 70*24*time.Hour + time.Minute, "pending", 70 * 24 * time.Hour},
		{"stale daily", "daily", 10*24*time.Hour + time.Hour, "missed", 10 * 24 * time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, owner, at := scheduleFixture(t)
			addSchedule(t, s, owner, at, "agent", tc.repeat)
			check(t, s.ScanDue(at.Add(tc.late)))
			_, runs := s.ListSchedules(owner)
			if len(runs) != 1 || runs[0].Status != tc.status || !runs[0].ScheduledAt.Equal(at.Add(tc.dueDelta)) {
				t.Fatalf("%+v", runs)
			}
			if tc.repeat != "once" {
				v, _ := s.ListSchedules(owner)
				if !v[0].NextRunAt.After(at.Add(tc.late)) {
					t.Fatal("next time not advanced")
				}
			}
		})
	}
}
func TestScheduleCivilTimeDST(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	check(t, err)
	tests := []struct {
		name            string
		at, after, want time.Time
	}{
		{"spring skip", time.Date(2026, 3, 7, 2, 30, 0, 0, loc), time.Date(2026, 3, 7, 2, 30, 0, 0, loc), time.Date(2026, 3, 9, 2, 30, 0, 0, loc)},
		{"fall wall time", time.Date(2026, 10, 31, 9, 0, 0, 0, loc), time.Date(2026, 10, 31, 9, 0, 0, 0, loc), time.Date(2026, 11, 1, 9, 0, 0, 0, loc)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := nextOccurrence(Schedule{At: tc.at, Repeat: "daily", Timezone: "America/New_York"}, tc.after)
			if !got.Equal(tc.want) {
				t.Fatalf("got %s want %s", got, tc.want)
			}
		})
	}
	// Whichever offset Go selects for a repeated 01:30, the next occurrence
	// after that one must be on the following civil day, not the second 01:30.
	first := time.Date(2026, 11, 1, 1, 30, 0, 0, loc)
	next := nextOccurrence(Schedule{At: first, Repeat: "daily", Timezone: "America/New_York"}, first)
	if next.In(loc).Day() != 2 {
		t.Fatalf("duplicate fall occurrence: %s", next)
	}
}
func TestScheduleOverlapDeleteAndOwnership(t *testing.T) {
	s, owner, at := scheduleFixture(t)
	v := addSchedule(t, s, owner, at, "agent", "daily")
	check(t, s.ScanDue(at))
	run := s.PendingRuns()[0]
	claimed, err := s.ClaimRun(run.ID)
	check(t, err)
	if !claimed {
		t.Fatal("not claimed")
	}
	claimed, err = s.ClaimRun(run.ID)
	check(t, err)
	if claimed {
		t.Fatal("double claim")
	}
	check(t, s.ScanDue(at.Add(24*time.Hour)))
	_, runs := s.ListSchedules(owner)
	if len(runs) != 2 || runs[1].Status != "missed" {
		t.Fatalf("overlap: %+v", runs)
	}
	for _, other := range []platform.Message{
		{Platform: "telegram", ChatID: 20, TopicID: 7, UserID: 1},
		{Platform: "telegram", ChatID: 10, TopicID: 8, UserID: 1},
		{Platform: "telegram", ChatID: 10, TopicID: 7, UserID: 2},
	} {
		schedules, history := s.ListSchedules(other)
		if len(schedules) != 0 || len(history) != 0 {
			t.Fatal("owner leak")
		}
		if s.DeleteSchedule(other, v.ID) == nil {
			t.Fatal("cross-owner delete")
		}
	}
	check(t, s.DeleteSchedule(owner, v.ID))
	_, runs = s.ListSchedules(owner)
	if runs[0].Status != "running" {
		t.Fatal("delete changed active work")
	}
	addSchedule(t, s, owner, at.Add(48*time.Hour), "reminder", "once")
	check(t, s.ScanDue(at.Add(48*time.Hour)))
	schedules, _ := s.ListSchedules(owner)
	check(t, s.DeleteSchedule(owner, schedules[0].ID))
	if len(s.PendingRuns()) != 0 {
		t.Fatal("delete left pending run")
	}
}
func TestScheduleRecovery(t *testing.T) {
	s, owner, at := scheduleFixture(t)
	addSchedule(t, s, owner, at, "agent", "once")
	addSchedule(t, s, owner, at, "reminder", "once")
	check(t, s.ScanDue(at))
	for _, r := range s.PendingRuns() {
		_, err := s.ClaimRun(r.ID)
		check(t, err)
	}
	s, err := Open(s.dir, nil)
	check(t, err)
	check(t, s.RecoverRuns())
	check(t, s.RecoverRuns())
	_, runs := s.ListSchedules(owner)
	for _, r := range runs {
		if r.Kind == "agent" && (r.Status != "interrupted" || len(r.Parts) != 1) {
			t.Fatalf("%+v", r)
		}
		if r.Kind == "reminder" && r.Status != "pending" {
			t.Fatalf("%+v", r)
		}
	}
}
func TestScheduleFailedAtomicScanDoesNotAdvance(t *testing.T) {
	s, owner, at := scheduleFixture(t)
	addSchedule(t, s, owner, at, "agent", "once")
	path := filepath.Join(s.dir, "schedules.json")
	check(t, os.Rename(path, path+".saved"))
	check(t, os.Mkdir(path, 0700))
	if s.ScanDue(at) == nil {
		t.Fatal("write unexpectedly succeeded")
	}
	schedules, runs := s.ListSchedules(owner)
	if !schedules[0].NextRunAt.Equal(at) || len(runs) != 0 {
		t.Fatal("failed write mutated memory")
	}
	check(t, os.Remove(path))
	check(t, os.Rename(path+".saved", path))
	check(t, s.ScanDue(at))
	if len(s.PendingRuns()) != 1 {
		t.Fatal("retry lost occurrence")
	}
}
func TestScheduleValidationAndCorruption(t *testing.T) {
	s, owner, at := scheduleFixture(t)
	base := Schedule{Route: owner, Kind: "agent", Text: "work", At: at, Repeat: "once", Timezone: "Asia/Taipei"}
	for _, mutate := range []func(*Schedule){
		func(v *Schedule) { v.Kind = "shell" }, func(v *Schedule) { v.Text = "" }, func(v *Schedule) { v.Repeat = "hourly" }, func(v *Schedule) { v.Timezone = "bad/zone" }, func(v *Schedule) { v.At = at.Add(-time.Hour) },
	} {
		v := base
		mutate(&v)
		if _, err := s.CreateSchedule(v, at.Add(-time.Minute)); err == nil {
			t.Fatal("invalid schedule accepted")
		}
	}
	check(t, os.WriteFile(filepath.Join(s.dir, "schedules.json"), []byte(`{"runs":[{"id":"schedule:x","status":"running","next":-1}]}`), 0600))
	if _, err := Open(s.dir, nil); err == nil {
		t.Fatal("corrupt queue accepted")
	}
}
func TestScheduledRepliesIndependentIDsAndDefer(t *testing.T) {
	s, owner, at := scheduleFixture(t)
	addSchedule(t, s, owner, at, "agent", "once")
	addSchedule(t, s, owner, at.Add(time.Second), "reminder", "once")
	check(t, s.ScanDue(at.Add(time.Minute)))
	runs := s.PendingRuns()
	for _, r := range runs {
		check(t, s.FinishRun(r.ID, "succeeded", []string{r.Kind}))
	}
	first, _ := s.Pending()
	check(t, s.DeferReply(first.ID))
	second, _ := s.Pending()
	if first.ID == second.ID {
		t.Fatal("scheduled replies collided or did not rotate")
	}
	check(t, s.Delivered(second.ID))
	check(t, s.Delivered(first.ID))
	if _, ok := s.Pending(); ok {
		t.Fatal("output remains")
	}
}

func TestScheduledDeliveryNotStarvedByFailedOrdinaryReply(t *testing.T) {
	s, owner, at := scheduleFixture(t)
	addSchedule(t, s, owner, at, "reminder", "once")
	check(t, s.ScanDue(at))
	run := s.PendingRuns()[0]
	check(t, s.FinishRun(run.ID, "succeeded", []string{"reminder"}))
	owner.UpdateID = 123
	check(t, s.Queue(owner, []string{"ordinary"}))
	first, _ := s.Pending()
	if first.ID != "telegram:123" {
		t.Fatal("unexpected ordinary reply")
	}
	check(t, s.DeferReply(first.ID))
	second, _ := s.Pending()
	if second.ID != run.ID {
		t.Fatal("failed ordinary reply blocked reminder")
	}
	check(t, s.DeferReply(second.ID))
	third, _ := s.Pending()
	if third.ID != first.ID {
		t.Fatal("scheduled reply blocked ordinary reply")
	}
	check(t, s.Delivered(third.ID))
	fourth, _ := s.Pending()
	if fourth.ID != run.ID {
		t.Fatal("scheduled reply starved after success")
	}
}

func TestScheduleQueueBackpressureAndRetention(t *testing.T) {
	s, owner, at := scheduleFixture(t)
	addSchedule(t, s, owner, at, "agent", "daily")
	// Include undelivered terminal output in backpressure, not only agent runs.
	s.schedules.Runs = make([]ScheduledRun, maxQueuedRuns)
	for i := range s.schedules.Runs {
		s.schedules.Runs[i] = ScheduledRun{Status: "succeeded", Parts: []string{"unsent"}}
	}
	check(t, s.ScanDue(at))
	schedules, _ := s.ListSchedules(owner)
	if !schedules[0].NextRunAt.Equal(at) {
		t.Fatal("full queue advanced schedule")
	}
	s.schedules.Runs = nil
	check(t, s.ScanDue(at))
	if len(s.PendingRuns()) != 1 {
		t.Fatal("capacity recovery lost run")
	}
	next := s.cloneSchedules()
	next.Runs = append(next.Runs, make([]ScheduledRun, retainedRuns+10)...)
	for i := 1; i < len(next.Runs); i++ {
		next.Runs[i].Status = "missed"
	}
	check(t, s.saveSchedules(next))
	if len(s.schedules.Runs) != retainedRuns+1 || s.schedules.Runs[0].Status != "pending" {
		t.Fatal("retention pruned active work or exceeded bound")
	}
}
