package state

import (
	"context"
	"errors"
	"slices"
	"testing"

	"q-agent/internal/learning"
	"q-agent/internal/localtools"
)

func TestLearningClaimDraftAndRecovery(t *testing.T) {
	const first, second = "https://example.org/archive", "https://example.net/archive"
	const quote1 = "The public archive statement records adoption in 1950."
	const start = `{"claim":{"subject":"The measure","predicate":"was adopted in","object":"1950","relations":`
	for _, tc := range []struct {
		name, output, status string
		state                learning.RunState
		limit                int64
		interrupt            bool
		fetchFailure         bool
		cancel               bool
		uncertain            bool
	}{
		{name: "support and context", output: start + `["supports","context"]}}`, status: "hypothesis", state: learning.RunCompleted},
		{name: "opposition and crash recovery", output: start + `["supports","refutes"]}}`, status: "unresolved", state: learning.RunCompleted, interrupt: true},
		{name: "only background", output: start + `["context","context"]}}`, state: learning.RunUnresolved},
		{name: "abstention", output: `{"claim":null}`, state: learning.RunUnresolved},
		{name: "wrong relation count", output: start + `["supports"]}}`, state: learning.RunUnresolved},
		{name: "no excerpt on failed fetch", state: learning.RunUnresolved, fetchFailure: true},
		{name: "model budget exhausted", state: learning.RunBudgetExhausted, limit: 5},
		{name: "cancel model call", state: learning.RunCancelled, cancel: true},
		{name: "uncertain model call stays unresolved", state: learning.RunUnresolved, uncertain: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			c := NewLearningCoordinator(dir)
			cfg := learning.DefaultConfig()
			cfg.Enabled = true
			cfg.Permissions.AllowDomains = []string{"example.org", "example.net"}
			if tc.limit > 0 {
				cfg.PerQuestion.Requests = tc.limit
			}
			id, err := c.EnqueueResearch(ctx, cfg, "Public archive statement", 2)
			if err != nil {
				t.Fatal(err)
			}
			searchCalls, fetchCalls, modelCalls := 0, 0, 0
			quote2 := "The public archive statement records adoption in 1951."
			if tc.status == "hypothesis" {
				quote2 = "The public archive statement is stored with the annual reports."
			}
			var afterReservation storedLearningRun
			search := discoverySearch(func(context.Context, string, int) ([]learning.SearchHit, learning.Budget, error) {
				searchCalls++
				return []learning.SearchHit{{URI: first}, {URI: second}}, learning.Budget{Requests: 1}, nil
			})
			fetch := researchFetcher(func(_ context.Context, uri string, _ int64) (learning.FetchedDocument, learning.Budget, error) {
				fetchCalls++
				if tc.fetchFailure {
					return learning.FetchedDocument{}, learning.Budget{Requests: 2}, errors.New("fetch unavailable")
				}
				text := quote1
				if uri == second {
					text = quote2
				}
				return researchTestDocument(uri, text), learning.Budget{Requests: 2, Documents: 1}, nil
			})
			model := learningTestModel(func(callCtx context.Context, input learning.StructuredRequest) ([]byte, learning.Budget, error) {
				modelCalls++
				if input.Operation != "draft_claim" || len(input.References) != 2 ||
					input.References[0].Text != quote1 || input.References[1].Text != quote2 {
					t.Fatalf("unexpected model inputs: %+v", input)
				}
				if tc.cancel {
					if err := c.Cancel(ctx, id); err != nil || callCtx.Err() != context.Canceled {
						t.Fatalf("model cancellation: %v, context: %v", err, callCtx.Err())
					}
					return nil, learning.Budget{}, callCtx.Err()
				}
				if tc.uncertain {
					var err error
					afterReservation, err = c.load(id)
					if err != nil {
						t.Fatal(err)
					}
					return nil, learning.Budget{Requests: 1}, context.DeadlineExceeded
				}
				return []byte(tc.output), learning.Budget{Requests: 1, Tokens: 100}, nil
			})
			crash := errors.New("interrupted after writing the commit")
			if tc.interrupt {
				guard := c.repo.guard
				c.repo.guard = func(ctx context.Context, p learning.Partition, lease learning.Lease, op string, write func() error) error {
					if err := guard(ctx, p, lease, op, write); err != nil {
						return err
					}
					return crash
				}
			}
			allow := func(string) bool { return true }
			result, err := c.WorkResearchWithClaims(ctx, cfg, id, allow, search, fetch, localtools.LearningDocumentReader{}, model)
			if tc.interrupt {
				if !errors.Is(err, crash) || result.State != learning.RunCommitting || result.ClaimID != "" {
					t.Fatalf("interrupted run: %+v, %v", result, err)
				}
				c = NewLearningCoordinator(dir)
				result, err = c.WorkResearch(ctx, cfg, id, allow, search, fetch, localtools.LearningDocumentReader{}, nil)
			}
			if tc.uncertain {
				if err != nil || result.StopReason != "provider_failed" {
					t.Fatalf("initial model failure: %+v, %v", result, err)
				}
				// Restore the last durable checkpoint a process would see if
				// it stopped before storing the model outcome.
				afterReservation.ExpiresAt = nil
				if err := c.save(afterReservation); err != nil {
					t.Fatal(err)
				}
				c = NewLearningCoordinator(dir)
				result, err = c.WorkResearchWithClaims(ctx, cfg, id, allow, search, fetch, localtools.LearningDocumentReader{}, model)
				if result.StopReason != "interrupted_provider_call" {
					t.Fatalf("unexpected stop reason after restart: %+v", result)
				}
			}
			wantModelCalls := 1
			if tc.fetchFailure || tc.limit > 0 {
				wantModelCalls = 0
			}
			if result.State != tc.state || searchCalls != 1 || fetchCalls != 2 || modelCalls != wantModelCalls ||
				(tc.state == learning.RunBudgetExhausted && !errors.Is(err, ErrLearningBudget)) ||
				(tc.state != learning.RunBudgetExhausted && err != nil) {
				t.Fatalf("result = %+v, calls = %d/%d/%d, error = %v", result, searchCalls, fetchCalls, modelCalls, err)
			}
			data, err := c.repo.load("public")
			if err != nil {
				t.Fatal(err)
			}
			snap := data.Snapshot
			if tc.status == "" {
				if snap.Version != 0 || len(snap.Notes) != 0 || len(snap.Claims) != 0 || result.ClaimID != "" {
					t.Fatalf("unsuitable output published knowledge: %+v", snap)
				}
				return
			}
			if snap.Version != 1 || len(data.Receipts) != 1 || len(snap.Notes) != 1 || len(snap.Claims) != 1 ||
				len(snap.Evidence) != 2 || result.ClaimID != id || !slices.Equal(snap.Notes[0].ClaimIDs, []string{id}) {
				t.Fatalf("claim publication: %+v, result = %+v", snap, result)
			}
			claim := snap.Claims[0]
			if claim.Status != tc.status || claim.Confidence != 0 || len(claim.Evidence) != 2 ||
				claim.Evidence[0].Relation != "supports" || claim.Evidence[0].EvidenceID != snap.Evidence[0].ID ||
				claim.Evidence[1].EvidenceID != snap.Evidence[1].ID ||
				(tc.interrupt && claim.Evidence[1].Relation != "refutes") ||
				(!tc.interrupt && claim.Evidence[1].Relation != "context") {
				t.Fatalf("claim lost evidence relations: %+v", claim)
			}
			record, err := c.load(id)
			if err != nil || len(record.Run.Checkpoint.OpenQuestions) != 1 ||
				len(record.Run.Checkpoint.Fingerprints) != 2 {
				t.Fatalf("lost checkpoint metadata: %+v, %v", record.Run.Checkpoint, err)
			}
			for i, source := range snap.Sources {
				if !snap.Evidence[i].MatchesSourceExcerpt(source) {
					t.Fatalf("invalid locator for evidence %+v", snap.Evidence[i])
				}
			}
		})
	}
}
