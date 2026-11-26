package state

import (
	"cmp"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"q-agent/internal/learning"
)

func ListLearningFrontier(entries []learning.FrontierEntry, now time.Time, allow func(string) bool, origin, status string) []learning.FrontierEntry {
	result := make([]learning.FrontierEntry, 0, len(entries))
	for _, entry := range entries {
		q := entry.Question
		if (origin == "" || q.OriginType == origin) && (status == "all" || q.Status == status) &&
			allow(q.Text) && (status != "candidate" || (q.Score != nil &&
			(q.CooldownUntil == nil || !now.Before(*q.CooldownUntil)))) {
			result = append(result, entry)
		}
	}
	slices.SortFunc(result, func(a, b learning.FrontierEntry) int {
		return cmp.Or(cmp.Compare(b.Question.Priority, a.Question.Priority),
			cmp.Compare(frontierScore(b.Question), frontierScore(a.Question)),
			b.Question.UpdatedAt.Compare(a.Question.UpdatedAt), cmp.Compare(a.Question.ID, b.Question.ID))
	})
	return result
}

func frontierScore(q learning.Question) float64 {
	if q.Score == nil {
		return 0
	}
	return q.Score.Final
}

// ConversationFrontier updates a chat topic with authorized conversation leads.
// The caller holds the learning-directory lock.
func (r *LearningRepository) ConversationFrontier(ctx context.Context, reader learning.ConversationReader,
	query learning.ConversationQuery, now, retainSince time.Time, observe bool, allow func(string) bool) ([]learning.FrontierEntry, error) {
	if reader == nil || allow == nil || query.Scope.TopicID == nil || query.From.IsZero() || !query.From.Before(query.Until) {
		return nil, learning.ErrDenied
	}
	key, err := learningPartitionKey(learning.Partition{Scope: &query.Scope})
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	data, err := r.load(key)
	if err != nil {
		return nil, err
	}
	prior := data.Snapshot.Frontier
	if !retainSince.IsZero() {
		prior = slices.DeleteFunc(slices.Clone(prior), func(entry learning.FrontierEntry) bool {
			return entry.Question.UpdatedAt.Before(retainSince)
		})
	}
	query.From = maxTime(query.From, retainSince)
	turns, blocked, err := recentLearningTurns(ctx, data, reader, query, now)
	if err != nil {
		return nil, err
	}
	entries, changed := learning.ConversationCandidates(prior, turns, now.UTC(), allow)
	entries = slices.DeleteFunc(entries, func(entry learning.FrontierEntry) bool { return blocked[entry.Question.ID] })
	if observe {
		known := make(map[string]bool, len(prior))
		for _, entry := range prior {
			known[entry.Question.ID] = true
		}
		for i := range entries {
			if !known[entries[i].Question.ID] {
				entries[i].AuditRef = rand.Text()
			}
		}
	}
	if (changed || len(prior) != len(data.Snapshot.Frontier)) && !slices.EqualFunc(entries, data.Snapshot.Frontier, func(a, b learning.FrontierEntry) bool {
		return a.Question.ID == b.Question.ID && slices.Equal(a.Question.OriginRefs, b.Question.OriginRefs)
	}) {
		if len(prior) != len(data.Snapshot.Frontier) {
			if err := r.removeLearningCopies(key); err != nil {
				return nil, err
			}
		}
		if err := r.saveFrontier(ctx, key, data, entries); err != nil {
			return nil, err
		}
	}
	return entries, nil
}

// RecentLearningTurns reads eligible exchanges for topic discovery while
// honoring private frontier erasure. The caller supplies an authorized reader.
func (r *LearningRepository) RecentLearningTurns(ctx context.Context, reader learning.ConversationReader,
	query learning.ConversationQuery, now time.Time) ([]learning.ConversationTurn, error) {
	key, err := learningPartitionKey(learning.Partition{Scope: &query.Scope})
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	data, err := r.load(key)
	if err != nil {
		return nil, err
	}
	turns, _, err := recentLearningTurns(ctx, data, reader, query, now)
	return turns, err
}

func recentLearningTurns(ctx context.Context, data learningPartition, reader learning.ConversationReader,
	query learning.ConversationQuery, now time.Time) ([]learning.ConversationTurn, map[string]bool, error) {
	query.From = privateLearningAfter(data, query.From)
	var turns []learning.ConversationTurn
	if query.From.Before(query.Until) {
		var err error
		turns, err = reader.Recent(ctx, query)
		if err != nil {
			return nil, nil, err
		}
		turns = slices.DeleteFunc(turns, func(turn learning.ConversationTurn) bool {
			return turn.At.Before(query.From) || !turn.At.Before(query.Until)
		})
	}
	turns, blocked := filterDeletedLearning(data, turns, now)
	return turns, blocked, nil
}

func (r *LearningRepository) PublicFrontier(ctx context.Context, cfg learning.Config, now time.Time, allow func(string) bool) ([]learning.FrontierEntry, error) {
	if !cfg.Enabled || allow == nil {
		return nil, learning.ErrDenied
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	data, err := r.load("public")
	if err != nil {
		return nil, err
	}
	view := publicRecallView(data.Snapshot, cfg, allow, learning.RecallQuery{}, "", true, false)
	generated := learning.KnowledgeCandidates(view.current, now.UTC(), allow)
	entries := data.Snapshot.Frontier
	known := make(map[string]bool, len(entries))
	for _, entry := range entries {
		known[entry.Question.ID] = true
	}
	active := make(map[string]bool, len(generated))
	for _, entry := range generated {
		q := entry.Question
		active[q.ID] = true
		if len(entries) >= learning.MaxFrontierCandidates || known[q.ID] {
			continue
		}
		// Unchanged text from the same origin stays in cooldown even
		// when a source was revised or a relation was rewritten.
		if slices.ContainsFunc(entries, func(prior learning.FrontierEntry) bool {
			old := prior.Question
			return old.OriginType == q.OriginType && strings.EqualFold(old.Text, q.Text) &&
				(old.Status == "queued" || old.Status == "resolved" ||
					(old.CooldownUntil != nil && now.Before(*old.CooldownUntil)))
		}) {
			continue
		}
		if cfg.TelemetryEnabled {
			entry.AuditRef = rand.Text()
		}
		entries = append(entries, entry)
		known[q.ID] = true
	}
	if len(entries) != len(data.Snapshot.Frontier) {
		if err := r.saveFrontier(ctx, "public", data, entries); err != nil {
			return nil, err
		}
	}
	visible := make([]learning.FrontierEntry, 0, len(entries))
	for _, entry := range entries {
		if active[entry.Question.ID] {
			visible = append(visible, entry)
		}
	}
	if cfg.Recall.AuditEnabled && cfg.Recall.FrontierFeedback {
		applyRecallFeedback(filepath.Join(filepath.Dir(r.dir), "audit", "latest-recall.json"), now, view.current.Relations, visible)
	}
	return visible, nil
}

// Recall provides a small, temporary score bonus without changing candidate
// identity, origin quotas or the persisted base scores.
func applyRecallFeedback(path string, now time.Time, relations []learning.Relation, entries []learning.FrontierEntry) {
	file, err := os.Open(path)
	if err != nil {
		return
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, 2049))
	var event learning.Event
	if err != nil || len(raw) > 2048 || json.Unmarshal(raw, &event) != nil || event.Type != learning.RecallUsed ||
		!validAuditMetadata(event) || event.At.After(now) || event.At.Before(now.AddDate(0, 0, -7)) {
		return
	}
	relevant := make(map[string]bool, len(event.ClaimRefs))
	for _, id := range event.ClaimRefs {
		relevant[id] = true
	}
	for _, edge := range relations {
		if relevant[edge.ClaimID] {
			relevant[edge.ID] = true
		}
	}
	for i := range entries {
		q := &entries[i].Question
		if q.Score != nil && slices.ContainsFunc(q.OriginRefs, func(id string) bool { return relevant[id] }) {
			score := *q.Score
			score.Utility += 0.15
			ranked := learning.RankQuestion(score)
			q.Score = &ranked
		}
	}
}

func (r *LearningRepository) saveFrontier(ctx context.Context, key string, data learningPartition, entries []learning.FrontierEntry) error {
	data.Snapshot.Version++
	data.Snapshot.Frontier = entries
	return r.saveLearningPartition(ctx, key, data)
}

func (r *LearningRepository) saveLearningPartition(ctx context.Context, key string, data learningPartition) error {
	if data.Snapshot.Version == 0 {
		return learning.ErrVersionConflict
	}
	if err := data.Snapshot.ValidateReferences(); err != nil {
		return err
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	if len(raw) > maxLearningPartitionBytes {
		return ErrLearningPartitionFull
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return atomicBytes(filepath.Join(r.dir, key+".json"), raw)
}

type FrontierHandoff struct {
	CandidateID string `json:"candidate_id"`
	RunID       string `json:"run_id"`
}

// QueueFrontier hands one selected question to the public worker. The caller
// holds the learning-directory lock and authorizes the frontier scope.
// For a chat-scoped entry the operator must supply a separate public query.
func (c *LearningCoordinator) QueueFrontier(ctx context.Context, cfg learning.Config, partition learning.Partition,
	entries []learning.FrontierEntry, candidate, publicQuery string, now time.Time, allow func(string) bool) (FrontierHandoff, error) {
	result := FrontierHandoff{}
	key, err := learningPartitionKey(partition)
	if err != nil || !cfg.Enabled || allow == nil {
		return result, learning.ErrDenied
	}
	if candidate == "next" && !partition.Public {
		return result, learning.ErrDenied
	}
	selected := learning.SelectFrontier(entries, cfg.Selection, now, allow)
	reason := ""
	for _, item := range selected {
		if (candidate == "next" || candidate == item.Question.ID) && (partition.Public || item.Question.OriginType == "conversation") {
			candidate, reason = item.Question.ID, item.Reason
			break
		}
	}
	visible := slices.IndexFunc(entries, func(entry learning.FrontierEntry) bool { return entry.Question.ID == candidate })
	if visible < 0 || !allow(entries[visible].Question.Text) {
		return result, learning.ErrDenied
	}
	entry := entries[visible]
	if partition.Public {
		publicQuery = entry.Question.Text
	}
	publicQuery = strings.TrimSpace(publicQuery)
	if len(publicQuery) == 0 || len(publicQuery) > 2000 || !allow(publicQuery) ||
		cfg.Permissions.Excludes(learning.ConversationTurn{Transcript: publicQuery}) {
		return result, learning.ErrDenied
	}
	r := c.repo
	c.mu.Lock()
	defer c.mu.Unlock()
	r.mu.Lock()
	defer r.mu.Unlock()
	data, err := r.load(key)
	if err != nil {
		return result, err
	}
	index := slices.IndexFunc(data.Snapshot.Frontier, func(entry learning.FrontierEntry) bool {
		return entry.Question.ID == candidate
	})
	if index < 0 {
		return result, learning.ErrDenied
	}
	saved := &data.Snapshot.Frontier[index]
	if saved.Question.Text != entry.Question.Text {
		return result, learning.ErrVersionConflict
	}
	runID := frontierResearchID("run", key, candidate)
	planID := frontierResearchID("plan", key, candidate)
	result = FrontierHandoff{CandidateID: candidate, RunID: runID}
	if saved.RunID == runID {
		run, err := c.readAuthorizedResearch(ctx, cfg, runID, allow)
		if err != nil {
			return FrontierHandoff{}, err
		}
		if run.Question != publicQuery {
			return FrontierHandoff{}, learning.ErrVersionConflict
		}
		observeFrontierHandoff(ctx, cfg, r, *saved)
		return result, nil
	}
	if reason == "" || saved.Question.Status != "candidate" || saved.RunID != "" {
		return FrontierHandoff{}, learning.ErrDenied
	}
	// A process interrupted between the files retries the same run and plan IDs.
	if err := c.createLocked(ctx, "public", runID, planID, publicQuery, 2); err != nil {
		return FrontierHandoff{}, err
	}
	saved.RunID = runID
	if cfg.TelemetryEnabled && saved.AuditRef == "" {
		saved.AuditRef = rand.Text()
	}
	saved.Question.Status, saved.Question.UpdatedAt = "queued", now.UTC()
	saved.Transitions = append(saved.Transitions, learning.FrontierTransition{At: now.UTC(), Status: "queued", Reason: reason})
	if err := r.saveFrontier(ctx, key, data, data.Snapshot.Frontier); err != nil {
		return FrontierHandoff{}, err
	}
	observeFrontierHandoff(ctx, cfg, r, *saved)
	return result, nil
}

func observeFrontierHandoff(ctx context.Context, cfg learning.Config, repo *LearningRepository, saved learning.FrontierEntry) {
	if cfg.TelemetryEnabled {
		if err := repo.ExportFrontierAudit(ctx, []learning.FrontierEntry{saved}); err != nil {
			log.Print("learning frontier audit pending")
		}
	}
}

func frontierResearchID(kind, scope, candidate string) string {
	sum := sha256.Sum256(fmt.Appendf(nil, "%s:%s:%s", kind, scope, candidate))
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(sum[:16])
}

// RefreshFrontier copies terminal run outcomes into the candidate history.
func (c *LearningCoordinator) RefreshFrontier(ctx context.Context, partition learning.Partition,
	entries []learning.FrontierEntry) ([]learning.FrontierEntry, error) {
	if !slices.ContainsFunc(entries, func(entry learning.FrontierEntry) bool {
		return entry.RunID != "" && entry.Question.Status == "queued"
	}) {
		return entries, nil
	}
	key, err := learningPartitionKey(partition)
	if err != nil {
		return nil, err
	}
	r := c.repo
	r.mu.Lock()
	defer r.mu.Unlock()
	data, err := r.load(key)
	if err != nil {
		return nil, err
	}
	changed := false
	for i := range entries {
		entry := &entries[i]
		if entry.RunID == "" || entry.Question.Status != "queued" {
			continue
		}
		run, err := c.Get(ctx, entry.RunID)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if !terminalLearningRun(run.Run.State) {
			continue
		}
		index := slices.IndexFunc(data.Snapshot.Frontier, func(saved learning.FrontierEntry) bool {
			return saved.Question.ID == entry.Question.ID && saved.RunID == entry.RunID
		})
		if index < 0 {
			return nil, learning.ErrVersionConflict
		}
		status := "unresolved"
		if run.Run.State == learning.RunCompleted && run.Run.CoverageScore >= 1 && entry.Question.OriginType != "gap" {
			status = "resolved"
		}
		at := run.Run.UpdatedAt
		entry.Question.Status, entry.Question.UpdatedAt = status, at
		next := at.AddDate(0, 0, 7)
		entry.Question.CooldownUntil = &next
		entry.Transitions = append(entry.Transitions, learning.FrontierTransition{At: at, Status: status, Reason: string(run.Run.State)})
		data.Snapshot.Frontier[index] = *entry
		changed = true
	}
	if changed {
		if err := r.saveFrontier(ctx, key, data, data.Snapshot.Frontier); err != nil {
			return nil, err
		}
	}
	return entries, nil
}
