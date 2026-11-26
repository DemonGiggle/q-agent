package runtime

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"q-agent/internal/learning"
	"q-agent/internal/localtools"
	"q-agent/internal/redaction"
	"q-agent/internal/state"
	"q-agent/internal/trust"
)

func (r *Runtime) dispatchNightResearch(ctx context.Context, now time.Time) {
	if !r.Learning.Enabled || !r.Learning.Schedule.Enabled || r.Learning.Schedule.Window(now) == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.nightResearch || r.nightResearchBusyLocked() || ctx.Err() != nil {
		return
	}
	r.nightResearch = true
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		defer func() {
			r.mu.Lock()
			r.nightResearch = false
			r.mu.Unlock()
		}()
		if err := r.runNightResearch(ctx, now, nil, nil); err != nil && ctx.Err() == nil {
			r.report(err)
		}
	}()
}

// The caller holds r.mu.
func (r *Runtime) nightResearchBusyLocked() bool {
	return r.closed || len(r.active) != 0 || len(r.queued) != 0 ||
		(r.Maintenance != nil && r.Maintenance.Busy())
}

func (r *Runtime) runNightResearch(ctx context.Context, now time.Time, search learning.SearchProvider, fetch learning.DocumentFetcher) error {
	unlock, err := state.Lock(filepath.Join(r.Config.StateDir, "learning"))
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return nil
	}
	if err != nil {
		return err
	}
	defer unlock()
	cfg := r.Learning
	window := cfg.Schedule.Window(now)
	if !cfg.Enabled || window == "" || cfg.Schedule.Window(now.Add(time.Minute)) != window {
		return nil
	}
	var chats []state.ChatRecordWithKey
	if cfg.Schedule.IdleMinutes > 0 || r.LearningTopics != nil {
		chats, err = r.Store.ListChats()
		if err != nil {
			return err
		}
	}
	if r.recentLearningChatActivity(chats, now) {
		return nil
	}
	r.mu.Lock()
	busy := r.nightResearchBusyLocked()
	r.mu.Unlock()
	if busy {
		return nil
	}
	if cfg.Permissions.BlocksDomain(localtools.LearningSearchDomain) {
		search = nil
	} else if search == nil {
		search = localtools.LearningSearch{Permissions: cfg.Permissions}
	}
	if fetch == nil {
		fetch = localtools.LearningFetch{Permissions: cfg.Permissions}
	}
	allow := func(text string) bool {
		return !strings.Contains(text, redaction.Marker) && !r.Options.Redaction.ContainsSecret(text) &&
			!trust.Suspicious(text) && !cfg.Permissions.Excludes(learning.ConversationTurn{Transcript: text})
	}
	workCtx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	var discover func(context.Context) ([]string, error)
	if r.LearningTopics != nil {
		discover = func(ctx context.Context) ([]string, error) {
			started := time.Now()
			discoveryCtx, cancel := context.WithTimeout(ctx, 25*time.Second)
			defer cancel()
			queries, err := r.discoverLearningTopics(discoveryCtx, now, chats, allow)
			if err != nil {
				return nil, err
			}
			if r.Learning.Schedule.IdleMinutes > 0 && len(queries) != 0 {
				currentChats, err := r.Store.ListChats()
				if err != nil {
					return nil, err
				}
				if r.recentLearningChatActivity(currentChats, now.Add(time.Since(started))) {
					return nil, nil
				}
			}
			r.mu.Lock()
			busy := r.nightResearchBusyLocked()
			r.mu.Unlock()
			if busy {
				return nil, nil
			}
			return queries, nil
		}
	}
	return state.NewLearningCoordinator(r.Config.StateDir).RunScheduledResearchWithDiscovery(
		workCtx, cfg, window, allow, search, fetch, localtools.LearningDocumentReader{}, discover)
}

func (r *Runtime) recentLearningChatActivity(chats []state.ChatRecordWithKey, now time.Time) bool {
	idleMinutes := r.Learning.Schedule.IdleMinutes
	if idleMinutes <= 0 {
		return false
	}
	for _, chat := range chats {
		if r.allowed(chat.WorkspaceRoute()) && !chat.Record.LastSeen.IsZero() &&
			now.Sub(chat.Record.LastSeen).Minutes() < float64(idleMinutes) {
			return true
		}
	}
	return false
}
