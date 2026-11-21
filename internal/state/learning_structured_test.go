package state

import (
	"context"
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
