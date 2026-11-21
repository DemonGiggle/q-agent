package state

import (
	"context"
	"crypto/rand"
	"errors"
	"testing"
	"time"

	"q-agent/internal/learning"
)

func TestLearningCoordinatorCommitRecovery(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	c := NewLearningCoordinator(dir)
	public := learning.Partition{Public: true}
	private := learning.Partition{Scope: &learning.Scope{Platform: "telegram", ChatID: 1, Visibility: learning.PrivateChat}}
	runID, planID, op := rand.Text(), rand.Text(), rand.Text()
	now := time.Now()
	c.now = func() time.Time { return now }

	if err := c.Create(ctx, public, runID, planID); err != nil {
		t.Fatal(err)
	}
	first, err := c.Acquire(ctx, runID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Acquire(ctx, runID, time.Minute); !errors.Is(err, ErrLearningLeaseHeld) {
		t.Fatalf("duplicate lease: %v", err)
	}
	for _, target := range []learning.RunState{learning.RunPlanned, learning.RunRunning, learning.RunValidating, learning.RunCommitting} {
		advanceLearningRun(t, c, first, target, op)
	}
	snapshot := learning.KnowledgeSnapshot{}
	if _, err := c.Repository().Commit(ctx, private, 0, first, op, snapshot); !errors.Is(err, learning.ErrDenied) {
		t.Fatalf("wrong partition: %v", err)
	}
	if _, err := c.Repository().Commit(ctx, public, 0, first, rand.Text(), snapshot); !errors.Is(err, learning.ErrDenied) {
		t.Fatalf("wrong operation: %v", err)
	}
	v, err := c.Repository().Commit(ctx, public, 0, first, op, snapshot)
	if v != 1 || err != nil {
		t.Fatalf("commit = %d, %v", v, err)
	}

	// Restart after the knowledge commit and before the completion checkpoint.
	c = NewLearningCoordinator(dir)
	now = now.Add(time.Minute)
	c.now = func() time.Time { return now }
	second, err := c.Acquire(ctx, runID, time.Minute)
	if err != nil || second.Fence <= first.Fence {
		t.Fatalf("new lease = %+v, %v", second, err)
	}
	if err := c.Release(ctx, first); !errors.Is(err, learning.ErrDenied) {
		t.Fatalf("old lease release: %v", err)
	}
	if _, err := c.Repository().Commit(ctx, public, 0, first, op, snapshot); !errors.Is(err, learning.ErrDenied) {
		t.Fatalf("old worker commit: %v", err)
	}
	if err := c.Cancel(ctx, runID); !errors.Is(err, ErrLearningCommitted) {
		t.Fatalf("cancel after commit: %v", err)
	}
	if v, err := c.Repository().Commit(ctx, public, 0, second, op, snapshot); v != 1 || err != nil {
		t.Fatalf("retry = %d, %v", v, err)
	}
	advanceLearningRun(t, c, second, learning.RunCompleted, op)
	got, err := c.Get(ctx, runID)
	if err != nil || got.Run.State != learning.RunCompleted || got.Run.EndedAt == nil ||
		got.Run.Checkpoint.OperationID != op || got.Version != 6 {
		t.Fatalf("completed run = %+v, %v", got, err)
	}
	if _, err := c.Acquire(ctx, runID, time.Minute); !errors.Is(err, learning.ErrDenied) {
		t.Fatalf("terminal run acquired: %v", err)
	}
}

func TestLearningCoordinatorCancelBeforeCommit(t *testing.T) {
	ctx := context.Background()
	c := NewLearningCoordinator(t.TempDir())
	partition := learning.Partition{Public: true}
	runID, op := rand.Text(), rand.Text()
	if err := c.Create(ctx, partition, runID, rand.Text()); err != nil {
		t.Fatal(err)
	}
	lease, err := c.Acquire(ctx, runID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []learning.RunState{learning.RunPlanned, learning.RunRunning, learning.RunValidating, learning.RunCommitting} {
		advanceLearningRun(t, c, lease, target, op)
	}
	before, err := c.Get(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	next := before.Run
	next.State = learning.RunCompleted
	if err := c.Checkpoint(ctx, lease, before.Version, next); err == nil {
		t.Fatal("completed before knowledge commit")
	}
	if err := c.Checkpoint(ctx, lease, before.Version-1, before.Run); !errors.Is(err, learning.ErrVersionConflict) {
		t.Fatalf("stale checkpoint: %v", err)
	}
	for range 2 {
		if err := c.Cancel(ctx, runID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.Repository().Commit(ctx, partition, 0, lease, op, learning.KnowledgeSnapshot{}); !errors.Is(err, learning.ErrDenied) {
		t.Fatalf("commit after cancel: %v", err)
	}
	after, err := c.Get(ctx, runID)
	if err != nil || after.Version != before.Version+1 || after.Run.State != learning.RunCancelled ||
		after.Run.EndedAt == nil || after.Run.Checkpoint.OperationID != op {
		t.Fatalf("cancelled run = %+v, %v", after, err)
	}
}

func advanceLearningRun(t *testing.T, c *LearningCoordinator, lease learning.Lease, state learning.RunState, operation string) {
	t.Helper()
	ctx := context.Background()
	current, err := c.Get(ctx, lease.RunID)
	if err != nil {
		t.Fatal(err)
	}
	next := current.Run
	next.State = state
	if state == learning.RunCommitting {
		next.Checkpoint = &learning.Checkpoint{OperationID: operation}
	}
	if err := c.Checkpoint(ctx, lease, current.Version, next); err != nil {
		t.Fatalf("advance to %s: %v", state, err)
	}
}
