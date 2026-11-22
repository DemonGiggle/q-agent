package state

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"q-agent/internal/learning"
)

type learningTestModel func(context.Context, learning.StructuredRequest) ([]byte, learning.Budget, error)

func (m learningTestModel) Generate(ctx context.Context, req learning.StructuredRequest) ([]byte, learning.Budget, error) {
	return m(ctx, req)
}

func TestLearningStructuredRetryUsesBudget(t *testing.T) {
	ctx := context.Background()
	cfg := learning.DefaultConfig()
	cfg.Enabled = true
	for _, test := range []struct {
		budget int64
		calls  int
		ok     bool
	}{{1, 1, false}, {2, 2, true}} {
		c := NewLearningCoordinator(t.TempDir())
		plan := providerTestPlan(cfg.PerQuestion)
		plan.Budget.Requests = test.budget
		lease := startProviderRun(t, c, plan)
		req := learning.StructuredRequest{SchemaJSON: []byte(`{"type":"object","properties":{"note":{"type":"string"}},"required":["note"]}`)}
		calls := 0
		adapter := learningTestModel(func(context.Context, learning.StructuredRequest) ([]byte, learning.Budget, error) {
			calls++
			raw := `{"note":false}`
			if calls == 2 {
				raw = `{"note":"valid"}`
			}
			return []byte(raw), learning.Budget{Requests: 1}, nil
		})
		raw, err := c.GenerateStructured(ctx, lease, cfg, plan, adapter, req,
			learning.Budget{Requests: 1, WallTimeMillis: 1000}, time.Second)
		if calls != test.calls || (err == nil) != test.ok || (test.ok && string(raw) != `{"note":"valid"}`) {
			t.Fatalf("budget %d: calls=%d raw=%s error=%v", test.budget, calls, raw, err)
		}
	}
}

func TestLearningTelemetryCountsRejectedToolRequest(t *testing.T) {
	ctx := context.Background()
	cfg := learning.DefaultConfig()
	cfg.Enabled, cfg.TelemetryEnabled = true, true
	dir := t.TempDir()
	c := NewLearningCoordinator(dir)
	plan := providerTestPlan(cfg.PerQuestion)
	lease := startProviderRun(t, c, plan)
	model := learningTestModel(func(context.Context, learning.StructuredRequest) ([]byte, learning.Budget, error) {
		return nil, learning.Budget{Requests: 1}, learning.ErrResearchToolRequest
	})
	_, err := c.GenerateStructured(ctx, lease, cfg, plan, model, learning.StructuredRequest{SchemaJSON: []byte(`{"type":"object"}`)},
		learning.Budget{Requests: 1, WallTimeMillis: 1000}, time.Second)
	if !errors.Is(err, learning.ErrResearchToolRequest) {
		t.Fatalf("rejection was not preserved: %v", err)
	}
	audit := LearningAudit{Dir: filepath.Join(dir, "learning", "audit")}
	now := time.Now().UTC()
	report, err := audit.Metrics(ctx, now.Add(-time.Hour), now.Add(time.Second))
	if err != nil || report.Events[learning.SafetyBlocked] != 1 ||
		report.Events[learning.ModelCallReserved] != 1 || report.InvalidFiles != 0 {
		t.Fatalf("rejected tool request metrics: %+v, %v", report, err)
	}
}
