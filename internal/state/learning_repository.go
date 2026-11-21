package state

import (
	"context"
	"crypto/sha256"
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
	"q-agent/internal/platform"
)

const maxLearningPartitionBytes = 4 << 20

var ErrLearningPartitionFull = errors.New("learning partition exceeds storage limit")

// LearningLeaseGuard keeps the current lease valid through the commit. It
// calls write once while holding the coordinator's lock, or returns an error
// without calling it if the run is cancelled, expires, or belongs elsewhere.
type LearningLeaseGuard func(ctx context.Context, partition learning.Partition, lease learning.Lease, write func() error) error

// LearningRepository uses the host's single-owner state directory. Use one
// instance per serving process while holding the existing state directory
// lock. Its mutex is independent of chat state. Construction performs no I/O.
type LearningRepository struct {
	dir   string
	guard LearningLeaseGuard
	mu    sync.Mutex
}

func NewLearningRepository(stateDir string, guard LearningLeaseGuard) *LearningRepository {
	return &LearningRepository{dir: filepath.Join(stateDir, "learning", "knowledge"), guard: guard}
}

var _ learning.KnowledgeRepository = (*LearningRepository)(nil)

type learningReceipt struct {
	Version uint64   `json:"version"`
	Digest  [32]byte `json:"digest"`
	RunID   string   `json:"run_id"`
}

type learningPartition struct {
	Schema   string                     `json:"schema_version"`
	Snapshot learning.KnowledgeSnapshot `json:"snapshot"`
	Receipts map[string]learningReceipt `json:"receipts"`
}

func learningPartitionKey(partition learning.Partition) (string, error) {
	if partition.Public && partition.Scope == nil {
		return "public", nil
	}
	scope := partition.Scope
	if partition.Public || scope == nil || scope.Platform == "" || scope.ChatID == 0 ||
		(scope.ChatID > 0 && scope.Visibility != learning.PrivateChat) ||
		(scope.ChatID < 0 && scope.Visibility != learning.GroupChat) {
		return "", learning.ErrDenied
	}
	route := platform.Message{Platform: scope.Platform, ChatID: scope.ChatID}
	if scope.TopicID == nil {
		return platform.WorkspaceKey("learning:chat:"+route.WorkspaceKey(), 0), nil
	}
	return platform.WorkspaceKey("learning:topic:"+route.WorkspaceKey(), *scope.TopicID), nil
}

func (r *LearningRepository) load(key string) (learningPartition, error) {
	path := filepath.Join(r.dir, key+".json")
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return learningPartition{Schema: learning.SchemaVersion}, nil
	}
	if err != nil {
		return learningPartition{}, err
	}
	defer f.Close()
	body, err := io.ReadAll(io.LimitReader(f, maxLearningPartitionBytes+1))
	if err != nil {
		return learningPartition{}, err
	}
	if len(body) > maxLearningPartitionBytes {
		return learningPartition{}, ErrLearningPartitionFull
	}
	var data learningPartition
	if err := json.Unmarshal(body, &data); err != nil {
		return learningPartition{}, fmt.Errorf("decode learning partition: %w", err)
	}
	if data.Schema != learning.SchemaVersion || data.Snapshot.Version == 0 {
		return learningPartition{}, fmt.Errorf("invalid learning partition version")
	}
	return data, nil
}

func (r *LearningRepository) Snapshot(ctx context.Context, partition learning.Partition) (learning.KnowledgeSnapshot, error) {
	var empty learning.KnowledgeSnapshot
	if err := ctx.Err(); err != nil {
		return empty, err
	}
	key, err := learningPartitionKey(partition)
	if err != nil {
		return empty, err
	}
	// Atomic replacement gives a reader either complete version.
	data, err := r.load(key)
	return data.Snapshot, err
}

func (r *LearningRepository) Commit(ctx context.Context, partition learning.Partition, expectedVersion uint64, lease learning.Lease, operationID string, next learning.KnowledgeSnapshot) (uint64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	key, err := learningPartitionKey(partition)
	if err != nil {
		return 0, err
	}
	if r.guard == nil || !auditID.MatchString(lease.RunID) || lease.Fence == 0 || lease.ExpiresAt.IsZero() {
		return 0, learning.ErrDenied
	}
	if !auditID.MatchString(operationID) || expectedVersion == math.MaxUint64 {
		return 0, fmt.Errorf("invalid learning operation ID or version")
	}
	next.Version = expectedVersion + 1
	if err := next.ValidateReferences(); err != nil {
		return 0, err
	}
	body, err := json.Marshal(next)
	if err != nil {
		return 0, err
	}
	if len(body) > maxLearningPartitionBytes {
		return 0, ErrLearningPartitionFull
	}
	receipt := learningReceipt{Version: next.Version, Digest: sha256.Sum256(body), RunID: lease.RunID}
	var version uint64
	err = r.guard(ctx, partition, lease, func() error {
		r.mu.Lock()
		defer r.mu.Unlock()
		if err := ctx.Err(); err != nil {
			return err
		}
		data, err := r.load(key)
		if err != nil {
			return err
		}
		if saved, ok := data.Receipts[operationID]; ok {
			if saved != receipt {
				return learning.ErrVersionConflict
			}
			version = saved.Version
			return SyncDir(r.dir)
		}
		if data.Snapshot.Version != expectedVersion {
			return learning.ErrVersionConflict
		}
		// Embed the already encoded payload to avoid marshaling the full
		// knowledge snapshot a second time while holding the commit lock.
		if data.Receipts == nil {
			data.Receipts = make(map[string]learningReceipt)
		}
		data.Receipts[operationID] = receipt
		persisted, err := json.Marshal(struct {
			Schema   string                     `json:"schema_version"`
			Snapshot json.RawMessage            `json:"snapshot"`
			Receipts map[string]learningReceipt `json:"receipts"`
		}{data.Schema, body, data.Receipts})
		if err != nil {
			return err
		}
		if len(persisted) > maxLearningPartitionBytes {
			return ErrLearningPartitionFull
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if !time.Now().Before(lease.ExpiresAt) {
			return learning.ErrDenied
		}
		if err := atomicBytes(filepath.Join(r.dir, key+".json"), persisted); err != nil {
			return err
		}
		version = next.Version
		return nil
	})
	return version, err
}
