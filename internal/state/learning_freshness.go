package state

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math"
	"path/filepath"
	"slices"
	"time"

	"q-agent/internal/learning"
)

func dueLearningSource(snapshot learning.KnowledgeSnapshot, cfg learning.Config, allow func(string) bool, now time.Time) (learning.Source, bool) {
	cutoff := now.AddDate(0, 0, -cfg.SourcePolicy.RevalidateDays)
	refs := claimReferences(snapshot, cfg, allow)
	var chosen learning.Source
	var oldest time.Time
	for _, claim := range snapshot.Claims {
		if claim.Status != "supported" || !claimTextsAllowed(cfg, allow, claim) || !allClaimReferencesAllowed(refs, claim.Evidence) {
			continue
		}
		for _, link := range claim.Evidence {
			source := refs[link.EvidenceID].Source
			if link.Relation != "supports" || !source.Available() {
				continue
			}
			last := source.FetchedAt
			if source.Check != nil {
				last = source.Check.At
			}
			if !last.After(cutoff) && (chosen.ID == "" || last.Before(oldest) || last.Equal(oldest) && source.ID < chosen.ID) {
				chosen, oldest = source, last
			}
		}
	}
	return chosen, chosen.ID != ""
}

// The caller holds the learning-directory lock and has saved the window marker
// before entering. A crash after the debit can spend the slot without repeating a fetch.
func (c *LearningCoordinator) revalidateLearningSource(ctx context.Context, cfg learning.Config, allow func(string) bool, fetch learning.DocumentFetcher) error {
	data, err := c.repo.load("public")
	if err != nil {
		return err
	}
	now := c.now().UTC()
	source, ok := dueLearningSource(data.Snapshot, cfg, allow, now)
	if !ok {
		return nil
	}
	wait := time.Duration(min(int64(10_000), cfg.PerQuestion.WallTimeMillis)) * time.Millisecond
	path := filepath.Join(filepath.Dir(c.dir), "quota.json")
	quota, err := loadLearningQuota(path, now.Format(time.DateOnly))
	if err != nil {
		return err
	}
	charge := learning.Budget{Requests: researchFetchRequests, Documents: 1, WallTimeMillis: wait.Milliseconds()}
	deadline := now.Add(wait)
	if ctx.Err() != nil || deadline.Format(time.DateOnly) != quota.Date || !quota.reserve("source_revalidation", charge, cfg) {
		return ctx.Err()
	}
	if err := quota.save(path); err != nil {
		return err
	}
	callCtx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	doc, usage, fetchErr := fetch.Fetch(callCtx, source.CanonicalURI, learningDocumentLimit)
	if err := ctx.Err(); err != nil {
		return err
	}
	result := "inconclusive"
	if _, ok := (learning.Budget{}).Reserve(usage, charge); ok && callCtx.Err() == nil {
		switch {
		case errors.Is(fetchErr, learning.ErrSourceGone):
			result = "gone"
		case errors.Is(fetchErr, learning.ErrSourceWithdrawn):
			result = "withdrawn"
		case fetchErr == nil && doc.Source.CanonicalURI == source.CanonicalURI && len(doc.Body) > 0 && len(doc.Body) <= learningDocumentLimit:
			hash := sha256.Sum256(doc.Body)
			switch {
			case hex.EncodeToString(hash[:]) != source.ContentHash:
				result = "changed"
			case !cfg.SourcePolicy.Accepts(doc.Source, now):
				result = "aged"
			default:
				result = "unchanged"
			}
		}
	}
	return c.repo.recordSourceCheck(ctx, data, source, result, c.now().UTC())
}

func (r *LearningRepository) recordSourceCheck(ctx context.Context, data learningPartition, source learning.Source, result string, now time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	current, err := r.load("public")
	if err != nil {
		return err
	}
	if current.Snapshot.Version != data.Snapshot.Version || current.Snapshot.Version == math.MaxUint64 {
		return learning.ErrVersionConflict
	}
	next := data.Snapshot
	next.Sources = slices.Clone(next.Sources)
	next.Claims = slices.Clone(next.Claims)
	index := slices.IndexFunc(next.Sources, func(item learning.Source) bool { return item.ID == source.ID })
	check := &learning.SourceCheck{At: now, Result: result}
	if source.Check != nil {
		check.VerifiedAt = source.Check.VerifiedAt
	}
	if result == "unchanged" {
		check.VerifiedAt = &now
	}
	next.Sources[index].Check = check
	if !next.Sources[index].Available() {
		affected := make(map[string]bool)
		for _, evidence := range next.Evidence {
			if evidence.SourceID == source.ID {
				affected[evidence.ID] = true
			}
		}
		for i, claim := range next.Claims {
			if claim.Status != "supported" || !slices.ContainsFunc(claim.Evidence, func(link learning.EvidenceLink) bool {
				return link.Relation == "supports" && affected[link.EvidenceID]
			}) {
				continue
			}
			if claim.Revision == math.MaxUint64 {
				return learning.ErrVersionConflict
			}
			claim.Status, claim.Confidence = "stale", 0
			claim.ConfidenceReason = "Supporting source revalidation: " + result
			appendClaimRevision(&next, i, claim, "STALE", claim.ConfidenceReason, now)
		}
	}
	next.Version++
	return r.savePublicKnowledge(ctx, data, next)
}
