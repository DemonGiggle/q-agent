package state

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"q-agent/internal/learning"
)

const researchClaimSchema = `{
	"type":"object","required":["claim"],"additionalProperties":false,
	"properties":{"claim":{"anyOf":[{"type":"null"},{
		"type":"object","required":["subject","predicate","object","relations"],"additionalProperties":false,
		"properties":{
			"subject":{"type":"string","minLength":1,"maxLength":160},
			"predicate":{"type":"string","minLength":1,"maxLength":160},
			"object":{"type":"string","minLength":1,"maxLength":160},
			"relations":{"type":"array","minItems":1,"maxItems":2,"items":{"enum":["supports","refutes","context"]}}
		}
	}]}}
}`

func (c *LearningCoordinator) advanceResearchClaim(ctx context.Context, lease learning.Lease, cfg learning.Config,
	plan learning.Plan, allow func(string) bool, current learning.RunSnapshot, model learning.ModelProvider) error {
	run := current.Run
	cp := run.Checkpoint
	stop := func(reason string) error {
		run.State, run.StopReason, run.Checkpoint.NextIntent = learning.RunUnresolved, reason, ""
		return c.Checkpoint(ctx, lease, current.Version, run)
	}
	if run.Spent.Requests != cp.ResearchRequests || cp.ResearchRequests == 0 {
		return stop("interrupted_provider_call")
	}
	if !c.researchCitationAllowed(cfg, allow, run) {
		return stop("policy_changed")
	}
	if model == nil {
		return fmt.Errorf("resume this run with learning work --claims")
	}
	refs := checkpointResearchReferences(cp)
	if plan.EvidenceRequirements.MinIndependentSources > 1 && !plan.EvidenceRequirements.HasRequiredSources(refs) {
		return stop("insufficient_independent_sources")
	}
	var excerpts []learning.DocumentSpan
	for _, ref := range refs {
		if ref.Evidence == nil {
			return stop("insufficient_claim_evidence")
		}
		excerpts = append(excerpts, learning.DocumentSpan{Locator: ref.Evidence.Locator, Text: ref.Evidence.ExcerptOrSummary})
	}
	const tokenLimit = 2432
	ms := min(int64(15_000), cfg.PerQuestion.WallTimeMillis)
	raw, err := c.GenerateStructured(ctx, lease, cfg, plan, model, learning.StructuredRequest{
		Operation: "draft_claim", TokenLimit: tokenLimit, SchemaJSON: []byte(researchClaimSchema), References: excerpts,
	}, learning.Budget{Requests: 1, Tokens: tokenLimit, WallTimeMillis: ms}, time.Duration(ms)*time.Millisecond)
	if errors.Is(err, ErrLearningBudget) || ctx.Err() != nil {
		return err
	}
	latest, loadErr := c.Get(ctx, run.ID)
	if loadErr != nil {
		return errors.Join(err, loadErr)
	}
	current, run = latest, latest.Run
	if terminalLearningRun(run.State) {
		return nil
	}
	if err != nil && !errors.Is(err, ErrLearningStructuredOutput) {
		return stop("provider_failed")
	}
	claim := decodeResearchClaim(cfg, allow, run, refs, raw)
	if claim == nil {
		return stop("insufficient_claim_evidence")
	}
	run.Checkpoint.ResearchClaim, run.State, run.Checkpoint.NextIntent = claim, learning.RunValidating, "validate"
	return c.Checkpoint(ctx, lease, current.Version, run)
}

func decodeResearchClaim(cfg learning.Config, allow func(string) bool, run learning.Run,
	refs []learning.ResearchReference, raw []byte) *learning.Claim {
	var output struct {
		Claim *struct {
			Subject, Predicate, Object string
			Relations                  []string
		}
	}
	if json.Unmarshal(raw, &output) != nil || output.Claim == nil || len(output.Claim.Relations) != len(refs) {
		return nil
	}
	draft := output.Claim
	claim := learning.Claim{
		Record: run.Record, ID: run.ID, RevisedAt: run.UpdatedAt, Revision: 1,
		Subject: strings.TrimSpace(draft.Subject), Predicate: strings.TrimSpace(draft.Predicate),
		Object: strings.TrimSpace(draft.Object),
	}
	for i, relation := range draft.Relations {
		claim.Evidence = append(claim.Evidence, learning.EvidenceLink{EvidenceID: refs[i].Evidence.ID, Relation: relation})
	}
	claim.Status = researchClaimStatus(claim.Evidence)
	if !researchClaimAllowed(cfg, allow, run.ID, refs, claim) {
		return nil
	}
	return &claim
}

func researchClaimStatus(links []learning.EvidenceLink) string {
	status := ""
	for _, link := range links {
		switch link.Relation {
		case "refutes":
			return "unresolved"
		case "supports":
			status = "hypothesis"
		}
	}
	return status
}

func researchClaimAllowed(cfg learning.Config, allow func(string) bool, runID string,
	refs []learning.ResearchReference, claim learning.Claim) bool {
	if claim.ID != runID || claim.Revision != 1 || claim.Confidence != 0 ||
		claim.Status != researchClaimStatus(claim.Evidence) || len(claim.Evidence) != len(refs) || claim.Validate() != nil {
		return false
	}
	for _, value := range []string{claim.Subject, claim.Predicate, claim.Object} {
		if len(value) > 160 || !allow(value) {
			return false
		}
	}
	text := claim.Subject + " " + claim.Predicate + " " + claim.Object
	if !allow(text) || cfg.Permissions.Excludes(learning.ConversationTurn{Transcript: text}) {
		return false
	}
	for i, ref := range refs {
		if ref.Evidence == nil || ref.Evidence.ID != claim.Evidence[i].EvidenceID {
			return false
		}
	}
	return true
}
