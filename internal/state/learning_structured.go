package state

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"q-agent/internal/learning"
)

var ErrLearningStructuredOutput = errors.New("learning provider returned invalid structured output")

// GenerateStructured validates against a host-supplied schema. One invalid
// response allows one separately budgeted retry. Provider and context errors
// stop immediately because their outcomes may be uncertain.
func (c *LearningCoordinator) GenerateStructured(ctx context.Context, lease learning.Lease, cfg learning.Config,
	plan learning.Plan, model learning.ModelProvider, req learning.StructuredRequest, charge learning.Budget,
	timeout time.Duration) ([]byte, error) {
	const maxJSONBytes = 16 << 10
	if model == nil || len(req.SchemaJSON) == 0 || len(req.SchemaJSON) > maxJSONBytes {
		return nil, errors.New("invalid learning model or output schema")
	}
	var schema *jsonschema.Schema
	if err := json.Unmarshal(req.SchemaJSON, &schema); err != nil || schema == nil {
		return nil, errors.New("invalid learning output schema")
	}
	resolved, err := schema.Resolve(nil)
	if err != nil {
		return nil, errors.New("cannot resolve learning output schema")
	}
	for range 2 {
		var raw []byte
		err := c.CallProvider(ctx, lease, cfg, plan, charge, timeout, func(ctx context.Context) (learning.Budget, error) {
			var usage learning.Budget
			var err error
			raw, usage, err = model.Generate(ctx, req)
			return usage, err
		})
		if err != nil {
			return nil, err
		}
		var document any
		if len(raw) <= maxJSONBytes && json.Unmarshal(raw, &document) == nil && resolved.Validate(document) == nil {
			return raw, nil
		}
	}
	return nil, ErrLearningStructuredOutput
}
