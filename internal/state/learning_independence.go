package state

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"

	"golang.org/x/net/publicsuffix"
	"q-agent/internal/learning"
)

const multiSourceMaxAttempts = 3

func (c *LearningCoordinator) advanceMultiSource(ctx context.Context, lease learning.Lease, cfg learning.Config,
	plan learning.Plan, allow func(string) bool, search learning.SearchProvider, fetch learning.DocumentFetcher,
	reader learning.DocumentReader, current learning.RunSnapshot) error {
	run := current.Run
	cp := run.Checkpoint
	stop := func(reason string) error {
		run.State, run.StopReason, cp.NextIntent = learning.RunUnresolved, reason, ""
		return c.Checkpoint(ctx, lease, current.Version, run)
	}
	if len(cp.ResearchReferences) != 0 && !c.researchCitationAllowed(cfg, allow, run) {
		run.CoverageScore = 0
		return stop("policy_changed")
	}
	var source *learning.Source
	var evidence *learning.Evidence
	var candidates []string
	var err error
	switch cp.NextIntent {
	case "search":
		if run.Spent.Requests != 0 {
			return stop("interrupted_provider_call")
		}
		candidates, err = c.researchSearch(ctx, lease, cfg, plan, allow, search)
		candidates = independentResearchCandidates(candidates)
	case "fetch_multiple":
		attempt := cp.ResearchAttempt
		if attempt < 0 || len(cp.ResearchCandidates) > multiSourceMaxAttempts {
			return fmt.Errorf("invalid multi-source checkpoint")
		}
		if run.Spent.Requests != int64(1+2*attempt) {
			return stop("interrupted_provider_call")
		}
		if attempt >= len(cp.ResearchCandidates) {
			return stop("insufficient_independent_sources")
		}
		uri := cp.ResearchCandidates[attempt]
		if !cfg.Permissions.ResearchURI(uri) || !allow(uri) {
			return stop("policy_changed")
		}
		source, evidence, err = c.researchFetch(ctx, lease, cfg, plan, allow, fetch, reader, uri)
	default:
		return fmt.Errorf("invalid multi-source checkpoint intent")
	}
	if errors.Is(err, ErrLearningBudget) || ctx.Err() != nil {
		return err
	}
	latest, loadErr := c.Get(ctx, lease.RunID)
	if loadErr != nil {
		return errors.Join(err, loadErr)
	}
	current, run = latest, latest.Run
	cp = run.Checkpoint
	if terminalLearningRun(run.State) {
		return nil
	}
	if cp.NextIntent == "search" {
		if err != nil {
			return stop("provider_failed")
		}
		if len(candidates) == 0 {
			return stop("no_qualified_source")
		}
		cp.ResearchCandidates, cp.NextIntent = candidates, "fetch_multiple"
	} else {
		if run.Spent.Requests != int64(1+2*(cp.ResearchAttempt+1)) {
			return stop("provider_failed")
		}
		cp.ResearchAttempt++
		if err == nil && source != nil && evidence != nil {
			ref := learning.ResearchReference{Source: *source, Evidence: evidence}
			if independentResearchReference(ref, cp.ResearchReferences) {
				cp.ResearchReferences = append(cp.ResearchReferences, ref)
			}
		}
		target := plan.EvidenceRequirements.MinIndependentSources
		run.CoverageScore = float64(len(cp.ResearchReferences)) / float64(target)
		if plan.EvidenceRequirements.HasRequiredSources(cp.ResearchReferences) {
			run.State, cp.NextIntent = learning.RunValidating, "validate"
		} else if cp.ResearchAttempt == len(cp.ResearchCandidates) {
			return stop("insufficient_independent_sources")
		}
	}
	return c.Checkpoint(ctx, lease, current.Version, run)
}

func independentResearchCandidates(uris []string) []string {
	var selected []string
	seen := make(map[string]bool)
	for _, uri := range uris {
		site := researchSite(uri)
		if site == "" || seen[site] {
			continue
		}
		selected = append(selected, uri)
		if len(selected) == multiSourceMaxAttempts {
			break
		}
		seen[site] = true
	}
	return selected
}

func researchSite(uri string) string {
	parsed, err := url.Parse(uri)
	if err != nil {
		return ""
	}
	host := strings.ToLower(strings.TrimSuffix(parsed.Hostname(), "."))
	site, err := publicsuffix.EffectiveTLDPlusOne(host)
	if err != nil {
		return ""
	}
	return site
}

// These checks detect duplicate site, declared origin, body, or excerpt.
// Distinct references still need semantic review before supporting a claim.
func independentResearchReference(ref learning.ResearchReference, prior []learning.ResearchReference) bool {
	site := researchSite(ref.Source.CanonicalURI)
	if site == "" || ref.Evidence == nil {
		return false
	}
	normalize := func(s string) string { return strings.Join(strings.Fields(strings.ToLower(s)), " ") }
	origin := ref.Source.SyndicationOrigin
	return !slices.ContainsFunc(prior, func(other learning.ResearchReference) bool {
		otherSite := researchSite(other.Source.CanonicalURI)
		otherOrigin := other.Source.SyndicationOrigin
		return otherSite == site || (origin != "" && (origin == otherSite || origin == otherOrigin)) ||
			(otherOrigin != "" && otherOrigin == site) || other.Source.ContentHash == ref.Source.ContentHash ||
			(other.Evidence != nil && normalize(ref.Evidence.ExcerptOrSummary) == normalize(other.Evidence.ExcerptOrSummary))
	})
}
