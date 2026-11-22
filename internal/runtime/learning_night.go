package runtime

import (
	"context"
	"errors"
	"os"
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
	cfg, err := state.LoadLearningPolicy(r.Config.StateDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	window := cfg.Schedule.Window(now)
	if !cfg.Enabled || window == "" || cfg.Schedule.Window(now.Add(time.Minute)) != window {
		return nil
	}
	if cfg.Schedule.IdleMinutes > 0 {
		chats, err := r.Store.ListChats()
		if err != nil {
			return err
		}
		for _, chat := range chats {
			if r.allowed(chat.WorkspaceRoute()) && !chat.Record.LastSeen.IsZero() &&
				now.Sub(chat.Record.LastSeen).Minutes() < float64(cfg.Schedule.IdleMinutes) {
				return nil
			}
		}
	}
	r.mu.Lock()
	busy := r.nightResearchBusyLocked()
	r.mu.Unlock()
	if busy {
		return nil
	}
	if len(cfg.Permissions.AllowDomains) == 0 || !cfg.Permissions.AllowsDomain(localtools.LearningSearchDomain) {
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
	return state.NewLearningCoordinator(r.Config.StateDir).RunScheduledResearch(
		workCtx, cfg, window, allow, search, fetch, localtools.LearningDocumentReader{})
}
