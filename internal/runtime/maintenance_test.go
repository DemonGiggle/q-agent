package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"q-agent/internal/platform"
)

type maintenanceStub struct {
	calls  int
	action string
	busy   bool
	err    error
}

func (s *maintenanceStub) Request(_ context.Context, action string, _ platform.Message) error {
	s.calls++
	s.action = action
	return s.err
}
func (s *maintenanceStub) Busy() bool { return s.busy }

func TestMaintenanceAuthorizationAndDirectRouting(t *testing.T) {
	for _, tc := range []struct {
		name, text     string
		admin, sandbox bool
		chat, user     int64
		calls          int
	}{
		{name: "host update", text: "/update", admin: true, chat: 1, user: 1, calls: 1},
		{name: "sandbox restart", text: "/restart", admin: true, sandbox: true, chat: 1, user: 1, calls: 1},
		{name: "approved not admin", text: "/update", chat: 1, user: 1},
		{name: "group admin denied", text: "/restart", admin: true, chat: 10, user: 1},
		{name: "unapproved user denied", text: "/restart", admin: true, chat: 1, user: 2},
		{name: "arguments rejected", text: "/update --arch arm64", admin: true, chat: 1, user: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, w, p := fixture(t)
			r.Config.Telegram.ApprovedChats = append(r.Config.Telegram.ApprovedChats, 1)
			if tc.admin {
				r.Config.Telegram.AdminUsers = []int64{1}
			}
			r.Config.Sandbox.Enabled = tc.sandbox
			stub := &maintenanceStub{}
			r.Maintenance = stub
			m := input(50, tc.chat, 0, tc.user, tc.text)
			must(t, r.Handle(context.Background(), m))
			must(t, r.Handle(context.Background(), m)) // Telegram replay cannot restart twice.
			r.Wait()
			if stub.calls != tc.calls || w.calls != 0 || len(p.requests) != 0 {
				t.Fatalf("maintenance=%d workspace=%d model=%d", stub.calls, w.calls, len(p.requests))
			}
		})
	}
}

func TestMaintenanceGlobalBusyAndFailure(t *testing.T) {
	r, _, p := fixture(t)
	r.Config.Telegram.ApprovedChats = append(r.Config.Telegram.ApprovedChats, 1)
	r.Config.Telegram.AdminUsers = []int64{1}
	stub := &maintenanceStub{}
	r.Maintenance = stub
	p.block = true
	p.started = make(chan struct{}, 1)
	must(t, r.Handle(context.Background(), input(1, 10, 0, 1, "running in another chat")))
	<-p.started
	must(t, r.Handle(context.Background(), input(2, 1, 0, 1, "/restart")))
	if stub.calls != 0 {
		t.Fatal("maintenance interrupted active task")
	}
	reply, ok := r.Store.Pending()
	if !ok || !strings.Contains(reply.Parts[0], "Tasks are still running") {
		t.Fatalf("%+v", reply)
	}
	must(t, r.Store.Delivered(reply.ID))
	must(t, r.Handle(context.Background(), input(3, 10, 0, 1, "/cancel")))
	r.Wait()
	stub.err = errors.New("missing systemd manager")
	must(t, r.Handle(context.Background(), input(4, 1, 0, 1, "/restart")))
	if stub.calls != 1 {
		t.Fatal("request not attempted")
	}
}

func TestMaintenanceGateStopsNewAgentTasks(t *testing.T) {
	r, w, p := fixture(t)
	r.Maintenance = &maintenanceStub{busy: true}
	must(t, r.Handle(context.Background(), input(1, 10, 0, 1, "do not run")))
	r.Wait()
	if w.calls != 0 || len(p.requests) != 0 {
		t.Fatal("task started while updating")
	}
	reply, ok := r.Store.Pending()
	if !ok || !strings.Contains(reply.Parts[0], "maintenance is in progress") {
		t.Fatalf("%+v", reply)
	}
}

func TestMaintenanceGateRetainsScheduledWorkUntilReleased(t *testing.T) {
	r, w, p := fixture(t)
	stub := &maintenanceStub{busy: true}
	r.Maintenance = stub
	enqueue(t, r, input(0, 10, 0, 1, ""), "agent", time.Now())
	must(t, r.dispatchScheduled(context.Background()))
	r.Wait()
	if len(r.Store.PendingRuns()) != 1 || w.calls != 0 || len(p.requests) != 0 {
		t.Fatal("maintenance dropped or dispatched scheduled work")
	}
	stub.busy = false
	must(t, r.dispatchScheduled(context.Background()))
	r.Wait()
	if len(r.Store.PendingRuns()) != 0 || len(p.requests) != 1 {
		t.Fatal("queued scheduled work did not resume")
	}
}
