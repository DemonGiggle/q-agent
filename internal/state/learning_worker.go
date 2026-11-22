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
const researchFetchRequests = 2

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
	ResumeState   learning.RunState          `json:"resume_state,omitempty"`
	SummaryStatus string                     `json:"summary_status,omitempty"`
	CoverageScore float64                    `json:"coverage_score"`
	EvidenceIDs   []string                   `json:"evidence_ids,omitempty"`
	EvidenceID    string                     `json:"evidence_id,omitempty"`
	ClaimID       string                     `json:"claim_id,omitempty"`
	Spent         learning.Budget            `json:"spent_budget"`
	Transitions   []LearningWorkerTransition `json:"transitions"`
}

// ResearchStatus reads an operator-owned public run without acquiring a
// lease. Neither the question nor the fetched content is returned.
func (c *LearningCoordinator) ResearchStatus(ctx context.Context, cfg learning.Config, id string, allow func(string) bool) (LearningWorkerResult, error) {
	record, err := c.readAuthorizedResearch(ctx, cfg, id, allow)
	if err != nil {
		return LearningWorkerResult{}, err
	}
	return researchWorkerResult(record), nil
}

func (c *LearningCoordinator) readAuthorizedResearch(ctx context.Context, cfg learning.Config, id string,
	allow func(string) bool) (storedLearningRun, error) {
	if err := ctx.Err(); err != nil {
		return storedLearningRun{}, err
	}
	if !cfg.Enabled || allow == nil {
		return storedLearningRun{}, learning.ErrDenied
	}
	record, err := c.load(id)
	if err != nil {
		return storedLearningRun{}, err
	}
	if record.PartitionKey != "public" || record.Question == "" || !allow(record.Question) ||
		cfg.Permissions.Excludes(learning.ConversationTurn{Transcript: record.Question}) {
		return storedLearningRun{}, learning.ErrDenied
	}
	return record, nil
}

func researchWorkerResult(record storedLearningRun) LearningWorkerResult {
	run := record.Run
	result := LearningWorkerResult{RunID: run.ID, State: run.State, StopReason: run.StopReason,
		ResumeState: record.ResumeState, CoverageScore: run.CoverageScore, Spent: run.Spent, Transitions: record.Transitions}
	// Older checkpoints have a transition list without explicit event IDs.
	// Derive the same ID on each read without rewriting the history.
	for i := range result.Transitions {
		if result.Transitions[i].EventID == "" {
			result.Transitions[i].EventID = researchTransitionID(run.ID, i)
		}
	}
	if cp := run.Checkpoint; cp != nil {
		result.SummaryStatus = cp.ResearchSummaryStatus
		if run.State == learning.RunCompleted {
			if cp.ResearchClaim != nil {
				result.ClaimID = cp.ResearchClaim.ID
				if cp.PublishedClaimID != "" {
					result.ClaimID = cp.PublishedClaimID
				}
			}
			if cp.ResearchEvidence != nil {
				result.EvidenceID = cp.ResearchEvidence.ID
			}
			for _, ref := range cp.ResearchReferences {
				if ref.Evidence != nil {
					result.EvidenceIDs = append(result.EvidenceIDs, ref.Evidence.ID)
				}
			}
		}
	}
	return result
}

func researchTransitionID(runID string, index int) string {
	sum := sha256.Sum256(fmt.Appendf(nil, "%s:%d", runID, index))
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(sum[:16])
}

func (c *LearningCoordinator) EnqueueResearch(ctx context.Context, cfg learning.Config, query string, sourceTarget int) (string, error) {
	if sourceTarget != 1 && sourceTarget != 2 {
		return "", fmt.Errorf("learning research requires one or two sources")
	}
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
	if err := c.create(ctx, learning.Partition{Public: true}, id, rand.Text(), query, sourceTarget); err != nil {
		return "", err
	}
	return id, nil
}

// WorkResearch processes one explicitly authorized public candidate. The
// caller holds the learning directory lock and checks secrets under its
// current policy. Search results never become evidence or claims.
func (c *LearningCoordinator) WorkResearch(ctx context.Context, cfg learning.Config, id string, allow func(string) bool,
	search learning.SearchProvider, fetch learning.DocumentFetcher, reader learning.DocumentReader, model learning.ModelProvider) (LearningWorkerResult, error) {
	return c.workResearch(ctx, cfg, id, allow, search, fetch, reader, model, false, false)
}

func (c *LearningCoordinator) WorkResearchWithClaims(ctx context.Context, cfg learning.Config, id string, allow func(string) bool,
	search learning.SearchProvider, fetch learning.DocumentFetcher, reader learning.DocumentReader, model learning.ModelProvider) (LearningWorkerResult, error) {
	if model == nil {
		return LearningWorkerResult{}, fmt.Errorf("learning claims require a model")
	}
	return c.workResearch(ctx, cfg, id, allow, search, fetch, reader, model, true, false)
}

func (c *LearningCoordinator) workResearch(ctx context.Context, cfg learning.Config, id string, allow func(string) bool,
	search learning.SearchProvider, fetch learning.DocumentFetcher, reader learning.DocumentReader, model learning.ModelProvider,
	draftClaims, scheduled bool) (result LearningWorkerResult, retErr error) {
	if err := cfg.Validate(); err != nil {
		return result, err
	}
	if !cfg.Enabled || allow == nil || search == nil || fetch == nil || reader == nil {
		return result, learning.ErrDenied
	}
	record, err := c.readAuthorizedResearch(ctx, cfg, id, allow)
	if err != nil {
		return result, err
	}
	question := record.Question

	if terminalLearningRun(record.Run.State) {
		return researchWorkerResult(record), nil
	}
	if c.cancelRequested(id) {
		if err := c.applyResearchCancel(ctx, id); err != nil && !errors.Is(err, ErrLearningCommitted) {
			return result, err
		} else if err == nil {
			return c.ResearchStatus(ctx, cfg, id, allow)
		}
	}
	if record.Run.State == learning.RunPaused {
		return researchWorkerResult(record), nil
	}
	sourceTarget := max(1, record.SourceTarget)
	if sourceTarget == 2 && model != nil && !draftClaims {
		return result, fmt.Errorf("two-source research does not support single-excerpt summarization")
	}
	lease, err := c.Acquire(ctx, id, time.Minute)
	if err != nil {
		return result, err
	}
	defer func() {
		releaseCtx := context.WithoutCancel(ctx)
		if c.cancelRequested(id) {
			err := c.applyResearchCancel(releaseCtx, id)
			if err != nil && !errors.Is(err, ErrLearningCommitted) && !errors.Is(err, learning.ErrDenied) {
				retErr = errors.Join(retErr, err)
			}
		}
		if ctx.Err() != nil && !scheduled {
			latest, err := c.load(id)
			retErr = errors.Join(retErr, ctx.Err(), err)
			if err == nil && !terminalLearningRun(latest.Run.State) {
				if err := c.Cancel(releaseCtx, id); err != nil && !errors.Is(err, ErrLearningCommitted) {
					retErr = errors.Join(retErr, err)
				}
			}
		}
		releaseErr := c.Release(releaseCtx, lease)
		latest, err := c.load(id)
		if err == nil {
			result = researchWorkerResult(latest)
			if result.State == learning.RunCancelled && ctx.Err() == nil && errors.Is(retErr, context.Canceled) {
				retErr = nil
			}
		}
		retErr = errors.Join(retErr, releaseErr, err)
	}()

	workCtx, stopWork := context.WithCancel(ctx)
	defer stopWork()
	stopWatching := c.watchResearchCancel(workCtx, id, stopWork)
	defer stopWatching()

	plan := learning.Plan{
		Record: record.Run.Record, ID: record.Run.PlanID, Goal: question,
		QuestionID: platform.WorkspaceKey(strings.ToLower(strings.Join(strings.Fields(question), " ")), 0),
		Version:    1, Budget: cfg.PerQuestion, Subquestions: []string{question},
		Steps: []string{"discover", "fetch", "select_excerpt", "record_source"}, AllowedSourceClasses: []string{"public"},
		StopConditions: []string{"source_recorded", "independent_excerpts_recorded", "authority_source_recorded",
			"no_qualified_source", "insufficient_independent_sources", "insufficient_claim_evidence", "budget_exhausted", "cancelled"},
		EvidenceRequirements: cfg.ResearchRequirements(sourceTarget),
	}
	if draftClaims {
		plan.Steps = []string{"discover", "fetch", "select_excerpt", "draft_claim", "validate", "record_source"}
	}
	if record.PlanBudget != nil {
		plan.Budget = *record.PlanBudget
	}

	for {
		current, err := c.Get(workCtx, id)
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
			cp.OpenQuestions = []string{plan.QuestionID}
		case learning.RunPlanned:
			run.State = learning.RunRunning
		case learning.RunRunning:
			if cp.NextIntent == "draft_claim" {
				current.Run = run
				var claimModel learning.ModelProvider
				if draftClaims {
					claimModel = model
				}
				if err := c.advanceResearchClaim(workCtx, lease, cfg, plan, allow, current, claimModel); err != nil {
					return result, err
				}
				continue
			}
			if sourceTarget == 2 {
				current.Run = run
				if err := c.advanceMultiSource(workCtx, lease, cfg, plan, allow, search, fetch, reader, current, draftClaims); err != nil {
					return result, err
				}
				continue
			}
			if cp.NextIntent == "summarize" {
				if run.Spent.Requests != researchCheckpointRequests(cp, 1+researchFetchRequests) {
					// The previous model call may have completed. Keep the
					// source without repeating an uncheckpointed call.
					cp.ResearchSummaryStatus = "interrupted"
					run.State, cp.NextIntent = learning.RunValidating, "validate"
					break
				}
				if model == nil {
					return result, fmt.Errorf("resume this run with learning work --summarize")
				}
				if cp.ResearchEvidence == nil || !c.researchCitationAllowed(cfg, allow, run) {
					run.State, run.StopReason = learning.RunUnresolved, "policy_changed"
					break
				}
				summary, modelErr := c.researchSummary(workCtx, lease, cfg, plan, model, *cp.ResearchEvidence)
				if errors.Is(modelErr, ErrLearningBudget) || workCtx.Err() != nil {
					return result, modelErr
				}
				current, err = c.Get(workCtx, id)
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
				(cp.NextIntent == "fetch" && run.Spent.Requests != researchCheckpointRequests(cp, 1)) {
				run.State, run.StopReason = learning.RunUnresolved, "interrupted_provider_call"
				break
			}
			var uri string
			var source *learning.Source
			var evidence *learning.Evidence
			if cp.NextIntent == "search" {
				var candidates []string
				candidates, err = c.researchSearch(workCtx, lease, cfg, plan, allow, search)
				if len(candidates) != 0 {
					uri = candidates[0]
				}
			} else if cp.NextIntent == "fetch" {
				source, evidence, err = c.researchFetch(workCtx, lease, cfg, plan, allow, fetch, reader, cp.ResearchURI)
			} else {
				return result, fmt.Errorf("invalid research checkpoint intent")
			}
			if errors.Is(err, ErrLearningBudget) || workCtx.Err() != nil {
				return result, err
			}
			// Reservation changed the checkpoint version and usage.
			latest, getErr := c.Get(workCtx, id)
			if getErr != nil {
				return result, errors.Join(err, getErr)
			}
			if terminalLearningRun(latest.Run.State) {
				return result, nil
			}
			current, run = latest, latest.Run
			cp = run.Checkpoint
			cp.ResearchRequests = run.Spent.Requests
			if err != nil || (uri == "" && source == nil) {
				run.State, run.StopReason = learning.RunUnresolved, "no_qualified_source"
				if errors.Is(err, learning.ErrInvalidSourceContent) {
					run.StopReason = "invalid_source_content"
				} else if err != nil {
					run.StopReason = "provider_failed"
				}
			} else if source != nil {
				cp.ResearchSource, cp.ResearchEvidence, cp.NextIntent = source, evidence, "validate"
				cp.Fingerprints = append(cp.Fingerprints, source.ContentHash)
				switch {
				case draftClaims && evidence == nil:
					run.State, run.StopReason = learning.RunUnresolved, "insufficient_claim_evidence"
				case draftClaims:
					cp.NextIntent = "draft_claim"
				case model != nil && evidence != nil:
					cp.NextIntent = "summarize"
				default:
					if model != nil {
						cp.ResearchSummaryStatus = "no_excerpt"
					}
					run.State = learning.RunValidating
				}
			} else {
				cp.ResearchURI, cp.NextIntent = uri, "fetch"
			}
		case learning.RunValidating:
			if sourceTarget == 2 && !plan.EvidenceRequirements.HasRequiredSources(cp.ResearchReferences) {
				run.State, run.StopReason = learning.RunUnresolved, "insufficient_independent_sources"
				break
			}
			if !c.researchCitationAllowed(cfg, allow, run) {
				run.CoverageScore = 0
				run.State, run.StopReason = learning.RunUnresolved, "policy_changed"
				break
			}
			// The manual worker validates at most one excerpt from one
			// source. Coverage is evidence collection, not factual confidence.
			if sourceTarget == 1 && cp.ResearchEvidence != nil {
				run.CoverageScore = 1 / float64(plan.EvidenceRequirements.MinIndependentSources)
			}
			run.State, cp.NextIntent, cp.OperationID = learning.RunCommitting, "commit", rand.Text()
		case learning.RunCommitting:
			if err := c.commitResearch(workCtx, cfg, lease, allow, plan, run); err != nil {
				return result, err
			}
			current, err = c.Get(workCtx, id)
			if err != nil {
				return result, err
			}
			run, cp = current.Run, current.Run.Checkpoint
			if terminalLearningRun(run.State) {
				return result, nil
			}
			if cp.ResearchClaim != nil {
				snapshot, err := c.repo.Snapshot(workCtx, learning.Partition{Public: true})
				if err != nil {
					return result, err
				}
				at := slices.IndexFunc(snapshot.Notes, func(note learning.Note) bool { return note.ID == run.ID })
				if at < 0 || len(snapshot.Notes[at].ClaimIDs) != 1 {
					return result, fmt.Errorf("committed claim is missing from its research note")
				}
				cp.PublishedClaimID = snapshot.Notes[at].ClaimIDs[0]
			}
			run.State, run.StopReason = learning.RunCompleted, "source_recorded"
			if sourceTarget == 2 {
				run.StopReason = "independent_excerpts_recorded"
				if len(cp.ResearchReferences) == 1 {
					run.StopReason = "authority_source_recorded"
				}
			}
		default:
			return result, fmt.Errorf("invalid research worker state %q", run.State)
		}
		if terminalLearningRun(run.State) {
			cp.NextIntent = ""
		}
		if err := c.Checkpoint(workCtx, lease, current.Version, run); err != nil {
			return result, err
		}
	}
}

func researchCheckpointRequests(cp *learning.Checkpoint, legacy int64) int64 {
	if cp.ResearchRequests == 0 {
		return legacy
	}
	return cp.ResearchRequests
}

func (c *LearningCoordinator) researchSearch(ctx context.Context, lease learning.Lease, cfg learning.Config, plan learning.Plan,
	allow func(string) bool, search learning.SearchProvider) ([]string, error) {
	ms := min(int64(15_000), cfg.PerQuestion.WallTimeMillis)
	var hits []learning.SearchHit
	charge := learning.Budget{Requests: 1, WallTimeMillis: ms}
	err := c.callResearchHTTP(ctx, lease, cfg, plan, charge, func(ctx context.Context) (learning.Budget, error) {
		var usage learning.Budget
		var err error
		hits, usage, err = search.Search(ctx, plan.Goal, 5)
		return usage, err
	})
	if err != nil {
		return nil, err
	}
	var uris []string
	for _, hit := range hits[:min(5, len(hits))] {
		if cfg.Permissions.ResearchURI(hit.URI) && allow(hit.URI) {
			uris = append(uris, hit.URI)
			if plan.EvidenceRequirements.MinIndependentSources == 1 {
				break
			}
		}
	}
	return uris, nil
}

func (c *LearningCoordinator) researchFetch(ctx context.Context, lease learning.Lease, cfg learning.Config, plan learning.Plan,
	allow func(string) bool, fetch learning.DocumentFetcher, reader learning.DocumentReader, uri string) (*learning.Source, *learning.Evidence, error) {
	if !cfg.Permissions.ResearchURI(uri) || !allow(uri) {
		return nil, nil, nil
	}
	ms := min(int64(10_000), cfg.PerQuestion.WallTimeMillis)
	charge := learning.Budget{Requests: researchFetchRequests, Documents: 1, WallTimeMillis: ms, StorageBytes: 24 << 10}
	var doc learning.FetchedDocument
	err := c.callResearchHTTP(ctx, lease, cfg, plan, charge, func(ctx context.Context) (learning.Budget, error) {
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
		len(source.Publisher) > 253 || len(source.SyndicationOrigin) > 253 || !allow(source.SyndicationOrigin) ||
		cfg.Permissions.Excludes(learning.ConversationTurn{Transcript: source.SyndicationOrigin}) || source.License != "" || source.TrustClass != "public_reference" ||
		!allow(source.Title) || cfg.Permissions.Excludes(learning.ConversationTurn{Transcript: source.Title}) {
		return nil, nil, nil
	}
	if err := source.Validate(); err != nil {
		return nil, nil, err
	}
	if !cfg.SourcePolicy.Accepts(source, c.now()) {
		return nil, nil, nil
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
	allow func(string) bool, plan learning.Plan, run learning.Run) error {
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
	if !c.researchCitationAllowed(cfg, allow, run) ||
		(record.SourceTarget == 2 && !plan.EvidenceRequirements.HasRequiredSources(run.Checkpoint.ResearchReferences)) {
		return c.failResearchCommit(ctx, lease, "policy_changed")
	}
	cp := run.Checkpoint
	refs := checkpointResearchReferences(cp)
	partition := learning.Partition{Public: true}
	next, err := c.repo.Snapshot(ctx, partition)
	if err != nil {
		return err
	}
	note := learning.Note{
		Record: refs[0].Source.Record, ID: run.ID, Title: plan.Goal, Revision: 1, BodyRef: "note:" + run.ID,
		Summary: "Public source retrieved. Relevance and factual claims have not been reviewed.",
	}
	for _, ref := range refs {
		source := ref.Source
		index := slices.IndexFunc(next.Sources, func(prior learning.Source) bool { return prior.ID == source.ID })
		if index == -1 {
			next.Sources = append(next.Sources, source)
		} else if prior := next.Sources[index]; prior.ContentHash != source.ContentHash || prior.CanonicalURI != source.CanonicalURI {
			return c.failResearchCommit(ctx, lease, "source_identity_conflict")
		} else {
			if prior.Check != nil && !prior.Available() {
				if !source.FetchedAt.After(prior.Check.At) {
					return c.failResearchCommit(ctx, lease, "policy_changed")
				}
				next.Sources[index].Check = &learning.SourceCheck{At: source.FetchedAt, VerifiedAt: &source.FetchedAt, Result: "unchanged"}
			}
			if prior.PublishedAt == nil && source.PublishedAt != nil {
				next.Sources[index].PublishedAt = source.PublishedAt
			}
		}
		note.SourceIDs = append(note.SourceIDs, source.ID)
		if evidence := ref.Evidence; evidence != nil {
			if !slices.ContainsFunc(next.Evidence, func(prior learning.Evidence) bool { return prior.ID == evidence.ID }) {
				next.Evidence = append(next.Evidence, *evidence)
			}
			note.EvidenceIDs = append(note.EvidenceIDs, evidence.ID)
			note.Summary = "Public source retrieved with a matching excerpt. Factual claims have not been reviewed."
		}
	}
	if len(refs) > 1 {
		note.Summary = "Public sources retrieved with distinct excerpts. Factual claims have not been reviewed."
	}
	if cp.ResearchSummary != "" {
		note.Summary = "Unreviewed model summary of the cited excerpt: " + cp.ResearchSummary
	}
	if cp.ResearchClaim != nil {
		id := evolveResearchClaim(&next, cfg, allow, *cp.ResearchClaim, c.now())
		if id == "" {
			return c.failResearchCommit(ctx, lease, "policy_changed")
		}
		note.ClaimIDs = []string{id}
	}
	next.Notes = append(next.Notes, note)
	_, err = c.repo.Commit(ctx, partition, next.Version, lease, cp.OperationID, next)
	if errors.Is(err, ErrLearningPartitionFull) {
		return errors.Join(err, c.failResearchCommit(ctx, lease, "storage_limit"))
	}
	return err
}

func (c *LearningCoordinator) researchCitationAllowed(cfg learning.Config, allow func(string) bool, run learning.Run) bool {
	cp := run.Checkpoint
	if cp == nil || !run.Spent.Fits(cfg.PerQuestion) ||
		!run.Spent.Fits(cfg.PerRun) || !run.Spent.Fits(cfg.GlobalDaily) {
		return false
	}
	refs := checkpointResearchReferences(cp)
	if len(refs) == 0 {
		return false
	}
	now := c.now()
	for i, ref := range refs {
		source := ref.Source
		if !cfg.Permissions.ResearchURI(source.CanonicalURI) || !allow(source.CanonicalURI) ||
			!allow(source.Title) || cfg.Permissions.Excludes(learning.ConversationTurn{Transcript: source.Title}) ||
			!cfg.SourcePolicy.Accepts(source, now) {
			return false
		}
		evidence := ref.Evidence
		if len(refs) > 1 && (evidence == nil || !independentResearchReference(ref, refs[:i])) {
			return false
		}
		if evidence != nil && (!evidence.MatchesSourceExcerpt(source) || !allow(evidence.ExcerptOrSummary) ||
			cfg.Permissions.Excludes(learning.ConversationTurn{Transcript: evidence.ExcerptOrSummary})) {
			return false
		}
	}
	if cp.ResearchClaim != nil && !researchClaimAllowed(cfg, allow, run.ID, refs, *cp.ResearchClaim) {
		return false
	}
	return cp.ResearchSummary == "" || (cp.ResearchEvidence != nil && allow(cp.ResearchSummary) &&
		!cfg.Permissions.Excludes(learning.ConversationTurn{Transcript: cp.ResearchSummary}))
}

func checkpointResearchReferences(cp *learning.Checkpoint) []learning.ResearchReference {
	if len(cp.ResearchReferences) != 0 {
		return cp.ResearchReferences
	}
	if cp.ResearchSource != nil {
		return []learning.ResearchReference{{Source: *cp.ResearchSource, Evidence: cp.ResearchEvidence}}
	}
	return nil
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
