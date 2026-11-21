package state

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sync"
	"time"

	"q-agent/internal/learning"
)

const maxLearningRunBytes = 64 << 10

var (
	ErrLearningLeaseHeld = errors.New("learning run already leased")
	ErrLearningCommitted = errors.New("learning knowledge already committed")
)

// LearningCoordinator owns run state within the process that holds the
// existing single-owner lock for the learning state directory.
// Use one instance per serving process.
type LearningCoordinator struct {
	dir         string
	repo        *LearningRepository
	now         func() time.Time
	mu          sync.Mutex
	activeCalls map[string]context.CancelFunc
}

func NewLearningCoordinator(stateDir string) *LearningCoordinator {
	c := &LearningCoordinator{dir: filepath.Join(stateDir, "learning", "runs"), now: time.Now}
	c.repo = NewLearningRepository(stateDir, c.guard)
	return c
}

func (c *LearningCoordinator) Repository() learning.KnowledgeRepository { return c.repo }

var _ learning.RunCoordinator = (*LearningCoordinator)(nil)

type storedLearningRun struct {
	PlanBudget   *learning.Budget `json:"plan_budget,omitempty"`
	QuestionKey  string           `json:"question_key,omitempty"`
	PartitionKey string           `json:"partition_key"`
	Version      uint64           `json:"version"`
	Fence        uint64           `json:"fence"`
	ExpiresAt    *time.Time       `json:"expires_at,omitempty"`
	Run          learning.Run     `json:"run"`
}

func (c *LearningCoordinator) Create(ctx context.Context, partition learning.Partition, runID, planID string) error {
	if !auditID.MatchString(runID) || !auditID.MatchString(planID) {
		return fmt.Errorf("invalid learning run or plan ID")
	}
	key, err := learningPartitionKey(partition)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	existing, err := c.load(runID)
	if err == nil {
		if existing.Run.PlanID != planID || existing.PartitionKey != key {
			return learning.ErrVersionConflict
		}
		return SyncDir(c.dir)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	now := c.now().UTC()
	return c.save(storedLearningRun{PartitionKey: key, Version: 1, Run: learning.Run{
		Record: learning.Record{SchemaVersion: learning.SchemaVersion, CreatedAt: now, UpdatedAt: now},
		ID:     runID, PlanID: planID, State: learning.RunQueued,
	}})
}

func (c *LearningCoordinator) load(id string) (storedLearningRun, error) {
	var result storedLearningRun
	if !auditID.MatchString(id) {
		return result, fmt.Errorf("invalid learning run ID")
	}
	f, err := os.Open(filepath.Join(c.dir, id+".json"))
	if err != nil {
		return result, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxLearningRunBytes+1))
	if err != nil {
		return result, err
	}
	if len(raw) > maxLearningRunBytes {
		return result, fmt.Errorf("learning run exceeds storage limit")
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return result, fmt.Errorf("decode learning run: %w", err)
	}
	if result.Run.ID != id || result.Run.SchemaVersion != learning.SchemaVersion ||
		result.PartitionKey == "" || result.Version == 0 ||
		(result.ExpiresAt != nil && result.Fence == 0) {
		return result, fmt.Errorf("invalid learning run record")
	}
	return result, nil
}

func (c *LearningCoordinator) save(record storedLearningRun) error {
	raw, err := json.Marshal(record)
	if err != nil {
		return err
	}
	if len(raw) > maxLearningRunBytes {
		return fmt.Errorf("learning run exceeds storage limit")
	}
	return atomicBytes(filepath.Join(c.dir, record.Run.ID+".json"), raw)
}

func (c *LearningCoordinator) Get(ctx context.Context, runID string) (learning.RunSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return learning.RunSnapshot{}, err
	}
	record, err := c.load(runID)
	if err != nil {
		return learning.RunSnapshot{}, err
	}
	return learning.RunSnapshot{Version: record.Version, Run: record.Run}, nil
}

func (c *LearningCoordinator) Acquire(ctx context.Context, runID string, ttl time.Duration) (learning.Lease, error) {
	if ttl <= 0 || ttl > 24*time.Hour {
		return learning.Lease{}, fmt.Errorf("invalid learning lease duration")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return learning.Lease{}, err
	}
	record, err := c.load(runID)
	if err != nil {
		return learning.Lease{}, err
	}
	now := c.now().UTC()
	if terminalLearningRun(record.Run.State) || record.Fence == math.MaxUint64 {
		return learning.Lease{}, learning.ErrDenied
	}
	if record.ExpiresAt != nil && now.Before(*record.ExpiresAt) {
		return learning.Lease{}, ErrLearningLeaseHeld
	}
	expires := now.Add(ttl)
	record.Fence++
	record.ExpiresAt = &expires
	if err := c.save(record); err != nil {
		return learning.Lease{}, err
	}
	return learning.Lease{RunID: runID, Fence: record.Fence, ExpiresAt: expires}, nil
}

func (c *LearningCoordinator) owns(record storedLearningRun, lease learning.Lease) bool {
	return record.ExpiresAt != nil && record.Fence == lease.Fence &&
		record.ExpiresAt.Equal(lease.ExpiresAt) && c.now().Before(*record.ExpiresAt)
}

func (c *LearningCoordinator) Release(ctx context.Context, lease learning.Lease) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	record, err := c.load(lease.RunID)
	if err != nil {
		return err
	}
	if record.Fence != lease.Fence || lease.Fence == 0 {
		return learning.ErrDenied
	}
	if record.ExpiresAt == nil {
		if cancel := c.activeCalls[lease.RunID]; cancel != nil {
			cancel()
		}
		return SyncDir(c.dir)
	}
	if !record.ExpiresAt.Equal(lease.ExpiresAt) {
		return learning.ErrDenied
	}
	record.ExpiresAt = nil
	if err := c.save(record); err != nil {
		return err
	}
	if cancel := c.activeCalls[lease.RunID]; cancel != nil {
		cancel()
	}
	return nil
}

func (c *LearningCoordinator) guard(ctx context.Context, partition learning.Partition, lease learning.Lease, operationID string, write func() error) error {
	key, err := learningPartitionKey(partition)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	record, err := c.load(lease.RunID)
	if err != nil {
		return err
	}
	if !c.owns(record, lease) || record.PartitionKey != key ||
		record.Run.State != learning.RunCommitting || record.Run.Checkpoint == nil || record.Run.Checkpoint.OperationID != operationID {
		return learning.ErrDenied
	}
	return write()
}

func (c *LearningCoordinator) Checkpoint(ctx context.Context, lease learning.Lease, expectedVersion uint64, next learning.Run) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	record, err := c.load(lease.RunID)
	if err != nil {
		return err
	}
	if !c.owns(record, lease) || terminalLearningRun(record.Run.State) {
		return learning.ErrDenied
	}
	if record.Version != expectedVersion || record.Version == math.MaxUint64 {
		return learning.ErrVersionConflict
	}
	if c.activeCalls[lease.RunID] != nil {
		return ErrLearningProviderActive
	}
	prior := record.Run
	if next.ID != prior.ID || next.PlanID != prior.PlanID ||
		!learningRunTransition(prior.State, next.State) ||
		!prior.Spent.Fits(next.Spent) ||
		math.IsNaN(next.CoverageScore) || next.CoverageScore < 0 || next.CoverageScore > 1 {
		return fmt.Errorf("invalid learning run update")
	}
	// Preserve the commit operation across retries and completion. A worker
	// must persist it before it can change the knowledge partition.
	if prior.State == learning.RunCommitting {
		if prior.Checkpoint == nil || next.Checkpoint == nil || next.Checkpoint.OperationID != prior.Checkpoint.OperationID {
			return fmt.Errorf("cannot change learning commit operation")
		}
	}
	if next.State == learning.RunCommitting {
		if next.Checkpoint == nil || !auditID.MatchString(next.Checkpoint.OperationID) {
			return fmt.Errorf("learning commit requires a checkpoint with an operation ID")
		}
	}
	if prior.Checkpoint != nil && next.Checkpoint == nil {
		return fmt.Errorf("cannot discard a learning checkpoint")
	}
	if next.Checkpoint != nil {
		cp := *next.Checkpoint
		if cp.Spent != next.Spent {
			return fmt.Errorf("checkpoint and run accounting differ")
		}
		cp.Version = record.Version + 1
		next.Checkpoint = &cp
	}
	if next.State == learning.RunCompleted {
		committed, err := c.hasCommit(record)
		if err != nil {
			return err
		}
		if !committed {
			return fmt.Errorf("cannot complete before committing learning knowledge")
		}
	}
	if next.State == learning.RunFailed && prior.State == learning.RunCommitting {
		committed, err := c.hasCommit(record)
		if err != nil {
			return err
		}
		if committed {
			return ErrLearningCommitted
		}
	}
	now := c.now().UTC()
	next.Record = prior.Record
	if now.After(next.UpdatedAt) {
		next.UpdatedAt = now
	}
	next.StartedAt = prior.StartedAt
	if next.State == learning.RunRunning && next.StartedAt == nil {
		next.StartedAt = &next.UpdatedAt
	}
	next.EndedAt = nil
	if terminalLearningRun(next.State) {
		next.EndedAt = &next.UpdatedAt
		record.ExpiresAt = nil
	}
	record.Version++
	record.Run = next
	return c.save(record)
}

func (c *LearningCoordinator) Cancel(ctx context.Context, runID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	record, err := c.load(runID)
	if err != nil {
		return err
	}
	if record.Run.State == learning.RunCancelled {
		if cancel := c.activeCalls[runID]; cancel != nil {
			cancel()
		}
		return SyncDir(c.dir)
	}
	if terminalLearningRun(record.Run.State) || record.Version == math.MaxUint64 {
		return learning.ErrDenied
	}
	if record.Run.State == learning.RunCommitting {
		committed, err := c.hasCommit(record)
		if err != nil {
			return err
		}
		if committed {
			return ErrLearningCommitted
		}
	}
	if now := c.now().UTC(); now.After(record.Run.UpdatedAt) {
		record.Run.UpdatedAt = now
	}
	record.Run.State = learning.RunCancelled
	record.Run.StopReason = "cancelled"
	record.Run.EndedAt = &record.Run.UpdatedAt
	record.ExpiresAt = nil
	record.Version++
	if err := c.save(record); err != nil {
		return err
	}
	if cancel := c.activeCalls[runID]; cancel != nil {
		cancel()
	}
	return nil
}

// hasCommit is called with the coordinator lock held, just like the
// repository commit callback. A rename that won the race stays committed.
func (c *LearningCoordinator) hasCommit(record storedLearningRun) (bool, error) {
	if record.Run.Checkpoint == nil {
		return false, nil
	}
	data, err := c.repo.load(record.PartitionKey)
	if err != nil {
		return false, err
	}
	receipt, ok := data.Receipts[record.Run.Checkpoint.OperationID]
	if !ok || receipt.RunID != record.Run.ID {
		return false, nil
	}
	// The previous attempt may have renamed the partition before returning
	// a directory-sync error. Make the receipt durable before finishing.
	return true, SyncDir(c.repo.dir)
}

func terminalLearningRun(state learning.RunState) bool {
	switch state {
	case learning.RunCompleted, learning.RunFailed, learning.RunCancelled, learning.RunBudgetExhausted, learning.RunUnresolved:
		return true
	}
	return false
}

func learningRunTransition(from, to learning.RunState) bool {
	if from == to || to == learning.RunFailed {
		return true
	}
	switch from {
	case learning.RunQueued:
		return to == learning.RunPlanned
	case learning.RunPlanned:
		return to == learning.RunRunning
	case learning.RunRunning:
		return to == learning.RunValidating || to == learning.RunPaused ||
			to == learning.RunUnresolved || to == learning.RunBudgetExhausted
	case learning.RunPaused:
		return to == learning.RunRunning || to == learning.RunBudgetExhausted
	case learning.RunValidating:
		return to == learning.RunCommitting || to == learning.RunUnresolved || to == learning.RunBudgetExhausted
	case learning.RunCommitting:
		return to == learning.RunCompleted
	}
	return false
}
