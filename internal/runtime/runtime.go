// Package runtime coordinates authorization, tool execution, and history.
package runtime

import (
	"context"
	"fmt"
	"strings"
	"time"

	"q-agent/internal/agent"
	"q-agent/internal/config"
	"q-agent/internal/llm"
	"q-agent/internal/platform"
	"q-agent/internal/redaction"
	"q-agent/internal/state"
	"q-agent/internal/telegram"
	"sync"
)

type Workspaces interface {
	Ensure(context.Context, platform.Message) (string, error)
}
type ProviderFactory func(state.Conversation) (llm.Provider, error)
type ToolFactory func(string) agent.Tools
type Sink interface {
	Typing(context.Context, platform.Message) error
}
type task struct {
	topic         int64
	user          int64
	ctx           context.Context
	cancel        context.CancelFunc
	userCancelled bool
	stopping      bool
	finishing     bool
	pending       []platform.Message
}
type Runtime struct {
	Config         config.Config
	Store          *state.Store
	Workspaces     Workspaces
	Providers      ProviderFactory
	Tools          ToolFactory
	Sink           Sink
	Options        agent.Options
	Report         func(error)
	mu             sync.Mutex
	active         map[string]task
	wg             sync.WaitGroup
	closed         bool
	modelMenus     map[string]modelMenu
	scheduleCancel context.CancelFunc
	scheduleDone   chan struct{}
	scheduleWake   chan struct{}
}

func (r *Runtime) allowed(m platform.Message) bool {
	user, chat := false, false
	for _, id := range r.Config.Telegram.ApprovedUsers {
		user = user || m.UserID == id
	}
	for _, id := range r.Config.Telegram.ApprovedChats {
		chat = chat || m.ChatID == id
	}
	return user && chat && m.Platform == "telegram"
}
func (r *Runtime) report(err error) {
	if err != nil && r.Report != nil {
		r.Report(err)
	}
}

// logf writes to the console only via Options.Logger. It never reaches
// Telegram: Telegram output flows only through Store.Queue in reply().
func (r *Runtime) logf(format string, args ...any) {
	if r.Options.Logger == nil {
		return
	}
	r.Options.Logger.Printf(format, args...)
}

func preview(text string) string {
	text = strings.TrimSpace(text)
	text = strings.Join(strings.Fields(text), " ")
	const limit = 120
	if len(text) > limit {
		return text[:limit] + "…"
	}
	return text
}
func (r *Runtime) reply(m platform.Message, text string) error {
	return r.Store.Queue(m, telegram.Split(text))
}

// Handle commits the update before doing work. Authorized commands use the
// same path as prompts, and provisioning consumes the same concurrency slot.
// All progress logs here are console-only; user-visible replies go through reply().
func (r *Runtime) Handle(ctx context.Context, m platform.Message) error {
	accepted, err := r.Store.Accept(m.UpdateID)
	if err != nil {
		r.logf("telegram update=%d accept error: %q", m.UpdateID, r.Options.Redaction.Text(redaction.Terminal, err.Error()))
		return err
	}
	if !accepted {
		r.logf("telegram update=%d duplicate, skipping", m.UpdateID)
		return nil
	}
	if !r.allowed(m) {
		r.logf("telegram update=%d chat=%d topic=%d user=%d rejected (not approved)", m.UpdateID, m.ChatID, m.TopicID, m.UserID)
		return nil
	}
	r.logf("telegram update=%d chat=%d topic=%d user=%d text=%q", m.UpdateID, m.ChatID, m.TopicID, m.UserID, r.Options.Redaction.Text(redaction.Terminal, preview(m.Text)))
	if m.CallbackID != "" {
		return r.handleModelCallback(ctx, m)
	}
	if fields := strings.Fields(m.Text); len(fields) > 0 && (fields[0] == "/provider" || fields[0] == "/model" || fields[0] == "/think" || fields[0] == "/settings") {
		return r.chatSettings(m, fields)
	}
	if m.Text == "/models" {
		return r.showModels(ctx, m)
	}
	r.mu.Lock()
	if r.active == nil {
		r.active = map[string]task{}
	}
	if r.closed {
		r.mu.Unlock()
		return context.Canceled
	}
	key := m.WorkspaceKey()
	active, busy := r.active[key]
	switch m.Text {
	case "/status":
		text := r.statusLocked(m)
		r.mu.Unlock()
		return r.reply(m, text)
	case "/help":
		r.mu.Unlock()
		return r.reply(m, "q-agent commands\n/status — Show current runtime, process RSS memory, and safe settings (no model call).\n/models — Choose a model for this chat.\n/provider <name|default> — Choose this chat’s provider.\n/model <id|default> — Set this chat’s model.\n/think <level|auto|default> — Set this chat’s thinking.\n/settings [reset] — Show settings or restore defaults.\n/help — Show this help.\n/new — Clear this topic's conversation; keep workspace.\n/cancel — Cancel this topic's active task.\n/ask <prompt> — Ask the agent or steer your running task.\n\nSend follow-up text in the same topic to steer your task. /cancel stops the task and pending follow-ups.\n\nIn groups, address commands to @your_bot (e.g. /status@your_bot), mention the bot, or reply to it.")
	case "/cancel":
		if busy && active.topic == m.TopicID {
			active.userCancelled = true
			r.active[key] = active
			active.cancel()
			r.logf("telegram update=%d cancel accepted, stopping active task", m.UpdateID)
			r.mu.Unlock()
			return r.reply(m, "Cancellation requested.")
		}
		r.logf("telegram update=%d cancel with no active task", m.UpdateID)
		r.mu.Unlock()
		return r.reply(m, "No active task in this conversation.")
	case "/new":
		if busy && active.topic == m.TopicID {
			r.logf("telegram update=%d /new rejected, conversation busy", m.UpdateID)
			r.mu.Unlock()
			return r.reply(m, "This conversation is busy. Use /cancel before /new.")
		}
		err := r.Store.Reset(m.ConversationKey())
		r.mu.Unlock()
		if err != nil {
			r.logf("telegram update=%d /new reset error: %q", m.UpdateID, err.Error())
			return err
		}
		r.logf("telegram update=%d /new cleared conversation", m.UpdateID)
		return r.reply(m, "Conversation cleared. Workspace retained.")
	}
	limit := r.Config.Limits.MaxConcurrent
	if limit <= 0 {
		limit = 2
	}
	if busy && active.topic == m.TopicID && active.user == m.UserID {
		err := r.steerLocked(m, active)
		r.mu.Unlock()
		return err
	}
	if busy || len(r.active) >= limit {
		r.logf("telegram update=%d busy (workspace busy=%t active=%d/%d), replying busy", m.UpdateID, busy, len(r.active), limit)
		r.mu.Unlock()
		return r.reply(m, "Busy. Try again when the active task finishes.")
	}
	err = r.startLocked(ctx, m, "")
	r.mu.Unlock()
	return err
}

func (r *Runtime) run(ctx context.Context, m platform.Message, runID string) bool {
	stopTyping := func() {}
	if r.Sink != nil {
		stopTyping = startTyping(ctx, r.Sink, m)
	}
	defer stopTyping()
	runStart := time.Now()
	r.logf("run start update=%d chat=%d topic=%d", m.UpdateID, m.ChatID, m.TopicID)
	live := newLiveReporter(r.Sink, m, r.Options.Redaction)
	// Only announce provisioning if it is slow (real first-time setup).
	// The fast path (existing chat folder, e.g. right after /models)
	// skips straight to contacting the model to avoid confusion.
	preparingTimer := time.AfterFunc(800*time.Millisecond, func() { live.SetStatus("Working\u2026 preparing workspace") })
	answer := ""
	var runErr error
	root, err := r.Workspaces.Ensure(ctx, m)
	preparingTimer.Stop()
	if err != nil {
		runErr = fmt.Errorf("workspace provisioning: %w", err)
		r.logf("run update=%d provisioning error after=%s err=%q", m.UpdateID, time.Since(runStart).Truncate(time.Millisecond), r.Options.Redaction.Text(redaction.Terminal, runErr.Error()))
	} else {
		conversation, err := r.loadModelConversation(m)
		if err != nil {
			runErr = err
			r.logf("run update=%d load history error after=%s err=%q", m.UpdateID, time.Since(runStart).Truncate(time.Millisecond), r.Options.Redaction.Text(redaction.Terminal, err.Error()))
		} else {
			r.logf("run update=%d model=%q", m.UpdateID, conversation.Model)
			provider, err := r.Providers(conversation)
			if err != nil {
				runErr = err
			} else {
				if identity, ok := provider.(interface{ SessionIdentity() string }); ok {
					conversation.Identity = identity.SessionIdentity()
				}
				// Save routing identity before the first request, including empty history.
				if err = r.Store.Save(m.ConversationKey(), conversation); err != nil {
					runErr = err
					r.logf("run update=%d save identity error after=%s err=%q", m.UpdateID, time.Since(runStart).Truncate(time.Millisecond), r.Options.Redaction.Text(redaction.Terminal, err.Error()))
				} else {
					options := r.Options
					options.Model = conversation.Model
					options.Thinking = conversation.Thinking
					options.NextInput = func(final bool) []string { return r.takeSteer(m.WorkspaceKey(), final) }
					options.OnSteer = live.Steered
					live.SetStatus("Working\u2026 contacting " + conversation.Model)
					toolCount := 0
					options.OnStream = func(ev llm.StreamEvent) { live.StreamEvent(ev) }
					options.OnToolStart = func(name string) {
						toolCount++
						live.ToolStart(name, toolCount)
					}
					options.OnToolEnd = func(name string, toolErr error) { live.ToolEnd(name, toolCount, toolErr) }
					result, err := agent.Run(ctx, provider, &scheduleTools{base: r.Tools(root), runtime: r, owner: m}, conversation.Messages(), m.Text, options)
					answer = result.Answer
					runErr = err
					if err == nil {
						r.logf("run update=%d agent ok after=%s steps=%d tools=%d usage=%d/%d/%d answer_len=%d", m.UpdateID, time.Since(runStart).Truncate(time.Millisecond), result.ModelSteps, result.ToolCalls, result.Usage.InputTokens, result.Usage.OutputTokens, result.Usage.TotalTokens, len(answer))
						conversation.Pairs = append(conversation.Pairs, state.Pair{User: m.Text, Answer: answer, Steers: result.Steers})
						if err = r.Store.Save(m.ConversationKey(), conversation); err != nil {
							runErr = fmt.Errorf("save history: %w", err)
							r.logf("run update=%d save history error after=%s err=%q", m.UpdateID, time.Since(runStart).Truncate(time.Millisecond), r.Options.Redaction.Text(redaction.Terminal, err.Error()))
						}
					} else {
						r.logf("run update=%d agent error after=%s err=%q", m.UpdateID, time.Since(runStart).Truncate(time.Millisecond), r.Options.Redaction.Text(redaction.Terminal, err.Error()))
					}
				}
			}
		}
	}
	if runErr != nil {
		if ctx.Err() == context.Canceled {
			answer = "Task cancelled."
		} else if ctx.Err() == context.DeadlineExceeded {
			answer = "Task reached its deadline."
		} else {
			answer = "Task failed: " + r.Options.Redaction.Text(redaction.Remote, runErr.Error())
		}
		r.report(runErr)
		if pending := r.discardSteer(m.WorkspaceKey()); pending > 0 {
			answer += fmt.Sprintf("\n%d pending follow-up message(s) were not applied; please resend them.", pending)
		}
		r.logf("run update=%d failed after=%s err=%q", m.UpdateID, time.Since(runStart).Truncate(time.Millisecond), r.Options.Redaction.Text(redaction.Terminal, runErr.Error()))
	} else {
		r.logf("run update=%d done after=%s answer_len=%d", m.UpdateID, time.Since(runStart).Truncate(time.Millisecond), len(answer))
	}
	if runErr != nil {
		live.FinishError(answer)
	} else {
		live.FinishSuccess()
	}
	var replyErr error
	if runID == "" {
		replyErr = r.reply(m, answer)
	} else {
		status := "succeeded"
		if runErr != nil {
			status = "failed"
		}
		if ctx.Err() == context.Canceled {
			status = "interrupted"
			r.mu.Lock()
			if r.active[m.WorkspaceKey()].userCancelled {
				status = "cancelled"
			}
			r.mu.Unlock()
		}
		replyErr = r.Store.FinishRun(runID, status, telegram.Split(answer))
	}
	if err := replyErr; err != nil {
		r.logf("run update=%d reply queue error after=%s err=%q", m.UpdateID, time.Since(runStart).Truncate(time.Millisecond), r.Options.Redaction.Text(redaction.Terminal, err.Error()))
		r.report(err)
	}
	return runErr == nil && replyErr == nil
}
func (r *Runtime) Close() {
	r.mu.Lock()
	r.closed = true
	if r.scheduleCancel != nil {
		r.scheduleCancel()
	}
	for _, t := range r.active {
		t.cancel()
	}
	done := r.scheduleDone
	r.mu.Unlock()
	if done != nil {
		<-done
	}
	r.wg.Wait()
}
func (r *Runtime) Wait() { r.wg.Wait() }
