package state

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"testing"
	"time"

	"q-agent/internal/learning"
	"q-agent/internal/localtools"
)

func researchTestDocument(uri, excerpt string) learning.FetchedDocument {
	now := time.Now().UTC()
	return learning.FetchedDocument{MediaType: "text/plain", Body: []byte(excerpt), Source: learning.Source{
		Record: learning.Record{SchemaVersion: learning.SchemaVersion, CreatedAt: now, UpdatedAt: now},
		ID:     fmt.Sprintf("%x", sha256.Sum256([]byte(uri))), CanonicalURI: uri, FetchedAt: now,
		ContentHash: fmt.Sprintf("%x", sha256.Sum256([]byte(excerpt))), TrustClass: "public_reference",
	}}
}

func TestLearningWorkerRetriesBudgetedHTTP(t *testing.T) {
	const first = "https://example.org/reference"
	const second = "https://example.net/reference"
	for _, target := range []int{1, 2} {
		t.Run(fmt.Sprint(target), func(t *testing.T) {
			ctx := context.Background()
			c := NewLearningCoordinator(t.TempDir())
			cfg := learning.DefaultConfig()
			cfg.Enabled = true
			cfg.Permissions.AllowDomains = []string{"example.org", "example.net"}
			id, err := c.EnqueueResearch(ctx, cfg, "Public question", target)
			if err != nil {
				t.Fatal(err)
			}

			searchCalls, fetchCalls := 0, 0
			search := discoverySearch(func(context.Context, string, int) ([]learning.SearchHit, learning.Budget, error) {
				searchCalls++
				usage := learning.Budget{Requests: 1}
				if searchCalls == 1 {
					return nil, usage, learning.ErrRetryableHTTP
				}
				return []learning.SearchHit{{URI: first}, {URI: second}}, usage, nil
			})
			fetch := researchFetcher(func(_ context.Context, uri string, _ int64) (learning.FetchedDocument, learning.Budget, error) {
				fetchCalls++
				if fetchCalls == 1 {
					return learning.FetchedDocument{}, learning.Budget{Requests: 2}, learning.ErrRetryableHTTP
				}
				text := "An archive discusses the public question."
				if uri == second {
					text = "Independent library records address the public question."
				}
				return researchTestDocument(uri, text), learning.Budget{Requests: 2, Documents: 1}, nil
			})
			var model learning.ModelProvider
			if target == 1 {
				model = learningTestModel(func(context.Context, learning.StructuredRequest) ([]byte, learning.Budget, error) {
					return []byte(`{"summary":"An archive discusses the public question."}`), learning.Budget{Requests: 1}, nil
				})
			}
			res, err := c.WorkResearch(ctx, cfg, id, func(string) bool { return true }, search, fetch, localtools.LearningDocumentReader{}, model)
			evidenceCount := len(res.EvidenceIDs)
			if res.EvidenceID != "" {
				evidenceCount++
			}
			if err != nil || res.State != learning.RunCompleted || evidenceCount != target ||
				searchCalls != 2 || fetchCalls != target+1 || res.Spent.Requests != int64(6+target) ||
				(target == 1 && res.SummaryStatus != "generated") {
				t.Fatalf("run = %+v, search = %d, fetch = %d, error = %v", res, searchCalls, fetchCalls, err)
			}
		})
	}
}

func TestLearningWorkerResumesFromSavedHTTPRetry(t *testing.T) {
	for _, pending := range []bool{false, true} {
		t.Run(fmt.Sprintf("pending_fetch=%t", pending), func(t *testing.T) {
			const uri = "https://example.org/reference"
			ctx := context.Background()
			dir := t.TempDir()
			c := NewLearningCoordinator(dir)
			cfg := learning.DefaultConfig()
			cfg.Enabled = true
			cfg.Permissions.AllowDomains = []string{"example.org"}
			id, err := c.EnqueueResearch(ctx, cfg, "Public question", 1)
			if err != nil {
				t.Fatal(err)
			}
			record, err := c.load(id)
			if err != nil {
				t.Fatal(err)
			}
			record.Run.State = learning.RunRunning
			record.Run.Spent = learning.Budget{Requests: 2, WallTimeMillis: 30250}
			if pending {
				record.Run.Spent.Requests += 2 * researchFetchRequests
			}
			record.Run.Checkpoint = &learning.Checkpoint{
				NextIntent: "fetch", Spent: record.Run.Spent, ResearchURI: uri, ResearchRequests: 2,
			}
			if err := c.save(record); err != nil {
				t.Fatal(err)
			}

			search := discoverySearch(func(context.Context, string, int) ([]learning.SearchHit, learning.Budget, error) {
				t.Fatal("repeated a search with a saved response")
				return nil, learning.Budget{}, nil
			})
			fetches := 0
			fetch := researchFetcher(func(_ context.Context, uri string, _ int64) (learning.FetchedDocument, learning.Budget, error) {
				fetches++
				return researchTestDocument(uri, "This archive addresses the public question."), learning.Budget{Requests: 2, Documents: 1}, nil
			})
			c = NewLearningCoordinator(dir)
			res, err := c.WorkResearch(ctx, cfg, id, func(string) bool { return true }, search, fetch, localtools.LearningDocumentReader{}, nil)
			if !pending && (err != nil || res.State != learning.RunCompleted || fetches != 1 || res.Spent.Requests != 4) {
				t.Fatalf("resume = %+v, fetches = %d, error = %v", res, fetches, err)
			}
			if pending && (err != nil || res.State != learning.RunUnresolved || res.StopReason != "interrupted_provider_call" ||
				fetches != 0 || res.Spent != record.Run.Spent) {
				t.Fatalf("interrupted retry = %+v, fetches = %d, error = %v", res, fetches, err)
			}
		})
	}
}

func TestLearningConcurrentResearchCommitRecovery(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	dir := t.TempDir()
	c := NewLearningCoordinator(dir)
	cfg := learning.DefaultConfig()
	cfg.Enabled = true
	cfg.Permissions.AllowDomains = []string{"example.org"}
	gate := make(chan struct{})
	arrived := make(chan string, 2)
	guard := c.repo.guard
	c.repo.guard = func(ctx context.Context, p learning.Partition, l learning.Lease, op string, write func() error) error {
		arrived <- l.RunID
		select {
		case <-gate:
			return guard(ctx, p, l, op, write)
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	search := discoverySearch(func(_ context.Context, question string, _ int) ([]learning.SearchHit, learning.Budget, error) {
		return []learning.SearchHit{{URI: "https://example.org/" + question}}, learning.Budget{Requests: 1}, nil
	})
	fetch := researchFetcher(func(_ context.Context, uri string, _ int64) (learning.FetchedDocument, learning.Budget, error) {
		return researchTestDocument(uri, "The archive includes a record for this reference."),
			learning.Budget{Requests: 2, Documents: 1}, nil
	})
	allow := func(string) bool { return true }
	type outcome struct {
		result LearningWorkerResult
		err    error
	}
	results := make(chan outcome, 2)
	ids := make(map[string]bool, 2)
	for _, question := range []string{"record", "reference"} {
		id, err := c.EnqueueResearch(ctx, cfg, question, 1)
		if err != nil {
			t.Fatal(err)
		}
		ids[id] = true
		go func() {
			res, err := c.WorkResearch(ctx, cfg, id, allow, search, fetch, localtools.LearningDocumentReader{}, nil)
			results <- outcome{res, err}
		}()
	}
	// Both runs prepare commits against revision zero before either write.
	for range 2 {
		select {
		case id := <-arrived:
			if !ids[id] {
				t.Fatalf("unexpected lease at commit: %s", id)
			}
			ids[id] = false
		case result := <-results:
			t.Fatalf("worker returned before commit: %+v", result)
		case <-ctx.Done():
			t.Fatal("both runs did not reach the commit fence")
		}
	}
	close(gate)

	var retryID string
	completed := 0
	for range 2 {
		got := <-results
		switch {
		case got.err == nil && got.result.State == learning.RunCompleted:
			completed++
		case errors.Is(got.err, learning.ErrVersionConflict) && got.result.State == learning.RunCommitting:
			retryID = got.result.RunID
		default:
			t.Fatalf("concurrent worker = %+v", got)
		}
	}
	if completed != 1 || retryID == "" {
		t.Fatalf("completed = %d, pending commit = %q", completed, retryID)
	}
	restarted := NewLearningCoordinator(dir)
	res, err := restarted.WorkResearch(ctx, cfg, retryID, allow, search, fetch, localtools.LearningDocumentReader{}, nil)
	if err != nil || res.State != learning.RunCompleted || res.Spent.Requests != 3 {
		t.Fatalf("commit recovery = %+v, error = %v", res, err)
	}
	stored, err := restarted.repo.load("public")
	if err != nil || stored.Snapshot.Version != 2 || len(stored.Snapshot.Notes) != 2 || len(stored.Receipts) != 2 {
		t.Fatalf("knowledge after concurrent commits = %+v, error = %v", stored, err)
	}
	for _, note := range stored.Snapshot.Notes {
		if _, ok := ids[note.ID]; !ok || ids[note.ID] {
			t.Fatalf("duplicate or unexpected note = %+v", note)
		}
		ids[note.ID] = true
	}
}
