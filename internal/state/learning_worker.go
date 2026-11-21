package state

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"q-agent/internal/learning"
	"q-agent/internal/platform"
)

const learningDocumentLimit = 128 << 10

type LearningWorkerTransition struct {
	EventID string            `json:"event_id,omitempty"`
	Usage   *learning.Budget  `json:"spent_budget,omitempty"`
	State   learning.RunState `json:"state"`
	At      time.Time         `json:"at"`
	Reason  string            `json:"reason,omitempty"`
}

type LearningWorkerResult struct {
	RunID         string                     `json:"run_id"`
	State         learning.RunState          `json:"state"`
	StopReason    string                     `json:"stop_reason,omitempty"`
	SummaryStatus string                     `json:"summary_status,omitempty"`
	CoverageScore float64                    `json:"coverage_score"`
	EvidenceID    string                     `json:"evidence_id,omitempty"`
	Spent         learning.Budget            `json:"spent_budget"`
	Transitions   []LearningWorkerTransition `json:"transitions"`
}

// ResearchStatus reads an operator-owned public run without acquiring a
// lease. Neither the question nor the fetched content is returned.
func (c *LearningCoordinator) ResearchStatus(ctx context.Context, cfg learning.Config, id string, allow func(string) bool) (LearningWorkerResult, error) {
	if err := ctx.Err(); err != nil {
		return LearningWorkerResult{}, err
	}
	if !cfg.Enabled || allow == nil {
		return LearningWorkerResult{}, learning.ErrDenied
	}
	record, err := c.load(id)
	if err != nil {
		return LearningWorkerResult{}, err
	}
	if record.PartitionKey != "public" || record.Question == "" || !allow(record.Question) ||
		cfg.Permissions.Excludes(learning.ConversationTurn{Transcript: record.Question}) {
		return LearningWorkerResult{}, learning.ErrDenied
	}
	return researchWorkerResult(record), nil
}

func researchWorkerResult(record storedLearningRun) LearningWorkerResult {
	run := record.Run
	result := LearningWorkerResult{RunID: run.ID, State: run.State, StopReason: run.StopReason,
		CoverageScore: run.CoverageScore, Spent: run.Spent, Transitions: record.Transitions}
	// Older checkpoints have a transition list without explicit event IDs.
	// Derive the same ID on each read without rewriting the history.
	for i := range result.Transitions {
		if result.Transitions[i].EventID == "" {
			result.Transitions[i].EventID = researchTransitionID(run.ID, i)
		}
	}
	if cp := run.Checkpoint; cp != nil {
		result.SummaryStatus = cp.ResearchSummaryStatus
		if run.State == learning.RunCompleted && cp.ResearchEvidence != nil {
			result.EvidenceID = cp.ResearchEvidence.ID
		}
	}
	return result
}

func researchTransitionID(runID string, index int) string {
	sum := sha256.Sum256(fmt.Appendf(nil, "%s:%d", runID, index))
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(sum[:16])
}

func (c *LearningCoordinator) EnqueueResearch(ctx context.Context, cfg learning.Config, query string) (string, error) {
	query = strings.TrimSpace(query)
	if err := cfg.Validate(); err != nil {
		return "", err
	}
	if !cfg.Enabled || cfg.Permissions.Excludes(learning.ConversationTurn{Transcript: query}) {
		return "", learning.ErrDenied
	}
	if query == "" || len(query) > 2000 {
		return "", fmt.Errorf("learning research requires a 1–2000 byte public question")
	}
	id := rand.Text()
	if err := c.create(ctx, learning.Partition{Public: true}, id, rand.Text(), query); err != nil {
		return "", err
	}
	return id, nil
}

// WorkResearch processes one explicitly authorized public candidate. The
// caller holds the learning directory lock and checks secrets under its
// current policy. Search results never become evidence or claims.
func (c *LearningCoordinator) WorkResearch(ctx context.Context, cfg learning.Config, id string, allow func(string) bool,
	search learning.SearchProvider, fetch learning.DocumentFetcher, reader learning.DocumentReader, model learning.ModelProvider) (result LearningWorkerResult, retErr error) {
	if err := cfg.Validate(); err != nil {
		return result, err
	}
	if !cfg.Enabled || allow == nil || search == nil || fetch == nil || reader == nil {
		return result, learning.ErrDenied
	}
	record, err := c.load(id)
	if err != nil {
		return result, err
	}
	question := record.Question
	if record.PartitionKey != "public" || question == "" || !allow(question) ||
		cfg.Permissions.Excludes(learning.ConversationTurn{Transcript: question}) {
		return result, learning.ErrDenied
	}

	if terminalLearningRun(record.Run.State) {
		return researchWorkerResult(record), nil
	}
	lease, err := c.Acquire(ctx, id, time.Minute)
	if err != nil {
		return result, err
	}
	defer func() {
		releaseCtx := context.WithoutCancel(ctx)
		if ctx.Err() != nil {
			latest, err := c.load(id)
			retErr = errors.Join(retErr, ctx.Err(), err)
			if err == nil && !terminalLearningRun(latest.Run.State) {
				if err := c.Cancel(releaseCtx, id); err != nil && !errors.Is(err, ErrLearningCommitted) {
					retErr = errors.Join(retErr, err)
				}
			}
		}
		retErr = errors.Join(retErr, c.Release(releaseCtx, lease))
		latest, err := c.load(id)
		retErr = errors.Join(retErr, err)
		if err == nil {
			result = researchWorkerResult(latest)
		}
	}()

	plan := learning.Plan{
		Record: record.Run.Record, ID: record.Run.PlanID, Goal: question,
		QuestionID: platform.WorkspaceKey(strings.ToLower(strings.Join(strings.Fields(question), " ")), 0),
		Version:    1, Budget: cfg.PerQuestion,
		Steps: []string{"discover", "fetch", "select_excerpt", "record_source"}, AllowedSourceClasses: []string{"public"},
		StopConditions:       []string{"source_recorded", "no_qualified_source", "budget_exhausted", "cancelled"},
		EvidenceRequirements: learning.EvidenceRequirements{MinIndependentSources: 1},
	}
	if record.PlanBudget != nil {
		plan.Budget = *record.PlanBudget
	}

	for {
		current, err := c.Get(ctx, id)
		if err != nil {
			return result, err
		}
		run := current.Run
		if terminalLearningRun(run.State) {
			return result, nil
		}
		cp := run.Checkpoint
		if cp == nil {
			cp = &learning.Checkpoint{}
			run.Checkpoint = cp
		}
		switch run.State {
		case learning.RunQueued:
			run.State, cp.NextIntent = learning.RunPlanned, "search"
		case learning.RunPlanned:
			run.State = learning.RunRunning
		case learning.RunRunning:
			if cp.NextIntent == "summarize" {
				if run.Spent.Requests > 3 {
					// The previous model call may have completed. Keep the
					// source without repeating an uncheckpointed call.
					cp.ResearchSummaryStatus = "interrupted"
					run.State, cp.NextIntent = learning.RunValidating, "validate"
					break
				}
				if model == nil {
					return result, fmt.Errorf("resume this run with learning work --summarize")
				}
				if cp.ResearchEvidence == nil || !researchCitationAllowed(cfg, allow, run) {
					run.State, run.StopReason = learning.RunUnresolved, "policy_changed"
					break
				}
				summary, modelErr := c.researchSummary(ctx, lease, cfg, plan, model, *cp.ResearchEvidence)
				if errors.Is(modelErr, ErrLearningBudget) || ctx.Err() != nil {
					return result, modelErr
				}
				current, err = c.Get(ctx, id)
				if err != nil {
					return result, err
				}
				run, cp = current.Run, current.Run.Checkpoint
				if terminalLearningRun(run.State) {
					return result, nil
				}
				switch {
				case modelErr != nil || summary == "":
					cp.ResearchSummaryStatus = "unavailable"
				case !allow(summary) || cfg.Permissions.Excludes(learning.ConversationTurn{Transcript: summary}):
					cp.ResearchSummaryStatus = "excluded"
				default:
					cp.ResearchSummary, cp.ResearchSummaryStatus = summary, "generated"
				}
				run.State, cp.NextIntent = learning.RunValidating, "validate"
				break
			}
			// Calls reserve before dispatch. If the process stopped before
			// checkpointing an output, its call outcome is unknown.
			if (cp.NextIntent == "search" && run.Spent.Requests != 0) ||
				(cp.NextIntent == "fetch" && run.Spent.Requests != 1) {
				run.State, run.StopReason = learning.RunUnresolved, "interrupted_provider_call"
				break
			}
			var uri string
			var source *learning.Source
			var evidence *learning.Evidence
			if cp.NextIntent == "search" {
				uri, err = c.researchSearch(ctx, lease, cfg, plan, allow, search)
			} else if cp.NextIntent == "fetch" {
				source, evidence, err = c.researchFetch(ctx, lease, cfg, plan, allow, fetch, reader, cp.ResearchURI)
			} else {
				return result, fmt.Errorf("invalid research checkpoint intent")
			}
			if errors.Is(err, ErrLearningBudget) || ctx.Err() != nil {
				return result, err
			}
			// Reservation changed the checkpoint version and usage.
			latest, getErr := c.Get(ctx, id)
			if getErr != nil {
				return result, errors.Join(err, getErr)
			}
			if terminalLearningRun(latest.Run.State) {
				return result, nil
			}
			current, run = latest, latest.Run
			cp = run.Checkpoint
			if err != nil || (uri == "" && source == nil) {
				run.State, run.StopReason = learning.RunUnresolved, "no_qualified_source"
				if errors.Is(err, learning.ErrInvalidSourceContent) {
					run.StopReason = "invalid_source_content"
				} else if err != nil {
					run.StopReason = "provider_failed"
				}
			} else if source != nil {
				cp.ResearchSource, cp.ResearchEvidence, cp.NextIntent = source, evidence, "validate"
				if model != nil && evidence != nil {
					cp.NextIntent = "summarize"
				} else {
					if model != nil {
						cp.ResearchSummaryStatus = "no_excerpt"
					}
					run.State = learning.RunValidating
				}
			} else {
				cp.ResearchURI, cp.NextIntent = uri, "fetch"
			}
		case learning.RunValidating:
			if !researchCitationAllowed(cfg, allow, run) {
				run.State, run.StopReason = learning.RunUnresolved, "policy_changed"
				break
			}
			// The manual worker validates at most one excerpt from one
			// source. Coverage is evidence collection, not factual confidence.
			if cp.ResearchEvidence != nil {
				run.CoverageScore = 1 / float64(plan.EvidenceRequirements.MinIndependentSources)
			}
			run.State, cp.NextIntent, cp.OperationID = learning.RunCommitting, "commit", rand.Text()
		case learning.RunCommitting:
			if err := c.commitResearch(ctx, cfg, lease, allow, question, run); err != nil {
				return result, err
			}
			current, err = c.Get(ctx, id)
			if err != nil {
				return result, err
			}
			run, cp = current.Run, current.Run.Checkpoint
			if terminalLearningRun(run.State) {
				return result, nil
			}
			run.State, run.StopReason = learning.RunCompleted, "source_recorded"
		default:
			return result, fmt.Errorf("invalid research worker state %q", run.State)
		}
		if terminalLearningRun(run.State) {
			cp.NextIntent = ""
		}
		if err := c.Checkpoint(ctx, lease, current.Version, run); err != nil {
			return result, err
		}
	}
}

func (c *LearningCoordinator) researchSearch(ctx context.Context, lease learning.Lease, cfg learning.Config, plan learning.Plan,
	allow func(string) bool, search learning.SearchProvider) (string, error) {
	ms := min(int64(15_000), cfg.PerQuestion.WallTimeMillis)
	var hits []learning.SearchHit
	charge := learning.Budget{Requests: 1, WallTimeMillis: ms}
	err := c.CallProvider(ctx, lease, cfg, plan, charge, time.Duration(ms)*time.Millisecond, func(ctx context.Context) (learning.Budget, error) {
		var usage learning.Budget
		var err error
		hits, usage, err = search.Search(ctx, plan.Goal, 5)
		return usage, err
	})
	if err != nil {
		return "", err
	}
	for _, hit := range hits[:min(5, len(hits))] {
		if cfg.Permissions.ResearchURI(hit.URI) && allow(hit.URI) {
			return hit.URI, nil
		}
	}
	return "", nil
}

func (c *LearningCoordinator) researchFetch(ctx context.Context, lease learning.Lease, cfg learning.Config, plan learning.Plan,
	allow func(string) bool, fetch learning.DocumentFetcher, reader learning.DocumentReader, uri string) (*learning.Source, *learning.Evidence, error) {
	if !cfg.Permissions.ResearchURI(uri) || !allow(uri) {
		return nil, nil, nil
	}
	ms := min(int64(10_000), cfg.PerQuestion.WallTimeMillis)
	charge := learning.Budget{Requests: 2, Documents: 1, WallTimeMillis: ms, StorageBytes: 24 << 10}
	var doc learning.FetchedDocument
	err := c.CallProvider(ctx, lease, cfg, plan, charge, time.Duration(ms)*time.Millisecond, func(ctx context.Context) (learning.Budget, error) {
		var usage learning.Budget
		var err error
		doc, usage, err = fetch.Fetch(ctx, uri, learningDocumentLimit)
		return usage, err
	})
	if err != nil {
		return nil, nil, err
	}
	source := doc.Source
	if source.CanonicalURI != uri || len(source.Title) > 256 || len(source.ContentHash) != 64 ||
		len(source.Publisher) > 253 || source.License != "" || source.TrustClass != "public_reference" ||
		!allow(source.Title) || cfg.Permissions.Excludes(learning.ConversationTurn{Transcript: source.Title}) {
		return nil, nil, nil
	}
	if err := source.Validate(); err != nil {
		return nil, nil, err
	}
	if len(doc.Body) > learningDocumentLimit {
		return nil, nil, learning.ErrInvalidSourceContent
	}
	spans, err := reader.Read(ctx, doc)
	if err != nil {
		return nil, nil, err
	}
	evidence, err := learning.SelectSourceExcerpt(doc, spans, plan.Goal, func(quote string) bool {
		return allow(quote) && !cfg.Permissions.Excludes(learning.ConversationTurn{Transcript: quote})
	})
	if err != nil {
		return nil, nil, err
	}
	return &source, evidence, nil
}

func (c *LearningCoordinator) researchSummary(ctx context.Context, lease learning.Lease, cfg learning.Config, plan learning.Plan,
	model learning.ModelProvider, evidence learning.Evidence) (string, error) {
	const tokenLimit = 2432
	ms := min(int64(15_000), cfg.PerQuestion.WallTimeMillis)
	charge := learning.Budget{Requests: 1, Tokens: tokenLimit, WallTimeMillis: ms}
	raw, err := c.GenerateStructured(ctx, lease, cfg, plan, model, learning.StructuredRequest{
		Operation: "summarize_excerpt", TokenLimit: tokenLimit,
		SchemaJSON: []byte(`{"type":"object","properties":{"summary":{"type":"string","minLength":1,"maxLength":200}},"required":["summary"],"additionalProperties":false}`),
		References: []learning.DocumentSpan{{Locator: evidence.Locator, Text: evidence.ExcerptOrSummary}},
	}, charge, time.Duration(ms)*time.Millisecond)
	if err != nil {
		return "", err
	}
	var output struct{ Summary string }
	if err := json.Unmarshal(raw, &output); err != nil {
		return "", err
	}
	return strings.TrimSpace(output.Summary), nil
}

func (c *LearningCoordinator) commitResearch(ctx context.Context, cfg learning.Config, lease learning.Lease,
	allow func(string) bool, question string, run learning.Run) error {
	record, err := c.load(run.ID)
	if err != nil {
		return err
	}
	c.mu.Lock()
	committed, err := c.hasCommit(record)
	c.mu.Unlock()
	if committed || err != nil {
		return err
	}
	if !researchCitationAllowed(cfg, allow, run) {
		return c.failResearchCommit(ctx, lease, "policy_changed")
	}
	cp := run.Checkpoint
	source := cp.ResearchSource
	partition := learning.Partition{Public: true}
	next, err := c.repo.Snapshot(ctx, partition)
	if err != nil {
		return err
	}
	found := false
	for _, prior := range next.Sources {
		if prior.ID == source.ID {
			if prior.ContentHash != source.ContentHash || prior.CanonicalURI != source.CanonicalURI {
				return c.failResearchCommit(ctx, lease, "source_identity_conflict")
			}
			found = true
			break
		}
	}
	if !found {
		next.Sources = append(next.Sources, *source)
	}
	note := learning.Note{
		Record: source.Record, ID: run.ID, Title: question, Revision: 1,
		Summary:   "Public source retrieved. Relevance and factual claims have not been reviewed.",
		SourceIDs: []string{source.ID},
	}
	if evidence := cp.ResearchEvidence; evidence != nil {
		if !slices.ContainsFunc(next.Evidence, func(prior learning.Evidence) bool { return prior.ID == evidence.ID }) {
			next.Evidence = append(next.Evidence, *evidence)
		}
		note.EvidenceIDs = []string{evidence.ID}
		note.Summary = "Public source retrieved with a matching excerpt. Factual claims have not been reviewed."
		if cp.ResearchSummary != "" {
			note.Summary = "Unreviewed model summary of the cited excerpt: " + cp.ResearchSummary
		}
	}
	next.Notes = append(next.Notes, note)
	_, err = c.repo.Commit(ctx, partition, next.Version, lease, cp.OperationID, next)
	if errors.Is(err, ErrLearningPartitionFull) {
		return errors.Join(err, c.failResearchCommit(ctx, lease, "storage_limit"))
	}
	return err
}

func researchCitationAllowed(cfg learning.Config, allow func(string) bool, run learning.Run) bool {
	cp := run.Checkpoint
	if cp == nil || cp.ResearchSource == nil || !run.Spent.Fits(cfg.PerQuestion) {
		return false
	}
	source := cp.ResearchSource
	if !cfg.Permissions.ResearchURI(source.CanonicalURI) || !allow(source.CanonicalURI) ||
		!allow(source.Title) || cfg.Permissions.Excludes(learning.ConversationTurn{Transcript: source.Title}) {
		return false
	}
	evidence := cp.ResearchEvidence
	if cp.ResearchSummary != "" && (evidence == nil || !allow(cp.ResearchSummary) ||
		cfg.Permissions.Excludes(learning.ConversationTurn{Transcript: cp.ResearchSummary})) {
		return false
	}
	return evidence == nil || (evidence.MatchesSourceExcerpt(*source) && allow(evidence.ExcerptOrSummary) &&
		!cfg.Permissions.Excludes(learning.ConversationTurn{Transcript: evidence.ExcerptOrSummary}))
}

func (c *LearningCoordinator) failResearchCommit(ctx context.Context, lease learning.Lease, reason string) error {
	current, err := c.Get(ctx, lease.RunID)
	if err != nil {
		return err
	}
	current.Run.State, current.Run.StopReason = learning.RunFailed, reason
	current.Run.Checkpoint.NextIntent = ""
	return c.Checkpoint(ctx, lease, current.Version, current.Run)
}
