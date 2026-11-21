package learning

import (
	"context"
	"errors"
	"time"
)

var ErrVersionConflict = errors.New("learning version conflict")

// ErrRetryableHTTP identifies a completed HTTP response that permits one
// bounded retry of a read-only discovery or fetch.
var ErrRetryableHTTP = errors.New("temporary research HTTP response")

// Partition is resolved by the trusted host. The zero value is invalid;
// public storage and each authorized private scope have separate partitions.
type Partition struct {
	Public bool
	Scope  *Scope
}

// KnowledgeSnapshot is authoritative. Derived indexes are rebuildable.
// Changes and revision history commit together through a compare-and-swap.
type KnowledgeSnapshot struct {
	Version   uint64
	Topics    []Topic
	Frontier  []FrontierEntry
	Sources   []Source
	Evidence  []Evidence
	Claims    []Claim
	Notes     []Note
	Relations []Relation
	Revisions []RevisionEvent
}

type KnowledgeRepository interface {
	Snapshot(context.Context, Partition) (KnowledgeSnapshot, error)
	// Commit replaces the partition only at expectedVersion. Implementations
	// validate all references and the active lease before atomically changing
	// claims, revisions and the idempotency receipt.
	Commit(ctx context.Context, partition Partition, expectedVersion uint64, lease Lease, operationID string, next KnowledgeSnapshot) (uint64, error)
}

type SearchHit struct {
	URI     string `json:"url"`
	Title   string `json:"title"`
	Snippet string `json:"snippet,omitempty"`
}

type SearchProvider interface {
	Search(ctx context.Context, query string, limit int) ([]SearchHit, Budget, error)
}

type FetchedDocument struct {
	Source    Source
	MediaType string
	Body      []byte
}

type DocumentFetcher interface {
	Fetch(ctx context.Context, uri string, maxBytes int64) (FetchedDocument, Budget, error)
}

type DocumentSpan struct {
	Locator Locator
	Text    string
}

// DocumentReader extracts untrusted reference data. It has no tool executor.
type DocumentReader interface {
	Read(context.Context, FetchedDocument) ([]DocumentSpan, error)
}

type StructuredRequest struct {
	Operation  string
	TokenLimit int
	SchemaJSON []byte
	References []DocumentSpan
}

type ModelProvider interface {
	Generate(context.Context, StructuredRequest) (responseJSON []byte, usage Budget, err error)
}

type IndexProvider interface {
	Rebuild(context.Context, Partition, KnowledgeSnapshot) error
	Lookup(ctx context.Context, partition Partition, query string, limit int) ([]string, error)
}

// Scheduler wakes the coordinator. It does not own run state.
type Scheduler interface {
	Wakeups(context.Context, ScheduleConfig) (<-chan time.Time, error)
}

type Lease struct {
	RunID     string
	Fence     uint64
	ExpiresAt time.Time
}

type RunSnapshot struct {
	Version uint64
	Run     Run
}

type RunCoordinator interface {
	Create(ctx context.Context, partition Partition, runID, planID string) error
	Get(ctx context.Context, runID string) (RunSnapshot, error)
	Acquire(ctx context.Context, runID string, ttl time.Duration) (Lease, error)
	Checkpoint(ctx context.Context, lease Lease, expectedVersion uint64, run Run) error
	Cancel(ctx context.Context, runID string) error
	Release(context.Context, Lease) error
}
