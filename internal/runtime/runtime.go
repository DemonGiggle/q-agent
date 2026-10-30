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
	"q-agent/internal/process"
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
type Maintenance interface {
	Request(context.Context, string, platform.Message) error
	Busy() bool
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
	wake          chan struct{}
	interactive   func()
}
type Runtime struct {
	Config            config.Config
	Store             *state.Store
	Workspaces        Workspaces
	Providers         ProviderFactory
	Tools             ToolFactory
	Sink              Sink
	Options           agent.Options
	Report            func(error)
	Maintenance       Maintenance
	Version           string
	mu                sync.Mutex
	active            map[string]task
	shellPrompts      map[string]*shellDecision
	wg                sync.WaitGroup
	closed            bool
	modelMenus        map[string]modelMenu
	providerMenus     map[string]providerMenu
	companionMenus    map[string]companionMenu
	scheduleCancel    context.CancelFunc
	scheduleDone      chan struct{}
	scheduleWake      chan struct{}
	digestCancel      context.CancelFunc
	digestDone        chan struct{}
	digestWake        chan struct{}
	digestMu          sync.Mutex
	digestRetries     map[string]digestRetry
	digestGlobalRetry state.MemoryRetry
	proactiveCancel   context.CancelFunc
	proactiveDone     chan struct{}
	proactiveMu       sync.Mutex
}

func (r *Runtime) allowed(m platform.Message) bool {
	if m.Platform != "telegram" {
		return false
	}
	// Private 1:1 chat: ChatID equals UserID and that ID is listed once.
	if m.ChatID > 0 {
		if m.ChatID != m.UserID {
			return false
		}
		for _, id := range r.Config.Telegram.AllowedUsers {
			if m.ChatID == id {
				return true
			}
		}
		return false
	}
	// Group/channel chat (negative ID): any sender is allowed when the
	// group itself is listed. No per-user enumeration required.
	if m.ChatID < 0 {
		for _, id := range r.Config.Telegram.AllowedGroups {
			if m.ChatID == id {
				return true
			}
		}
	}
	return false
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

func (r *Runtime) reply(m platform.Message, text string) error {
	return r.Store.Queue(m, telegram.FormatAndSplit(text))
}

// replyAnswer queues the final agent answer, promoting Markdown images to
// native Telegram photos. Local paths resolve against the chat workspace root
// (sandbox rootfs or host workspace dir); remote https:// URLs pass through.
// Invalid images fall back to link/text with an explanation per ExtractPhotos.
func (r *Runtime) replyAnswer(m platform.Message, answer, root string) error {
	photos, cleaned, _ := telegram.ExtractPhotos(answer, root)
	var parts []string
	if len(photos) > 0 && len(strings.TrimSpace(cleaned)) == 0 {
		parts = nil
	} else {
		parts = telegram.FormatAndSplit(cleaned)
	}
	if len(photos) == 0 {
		return r.Store.Queue(m, parts)
	}
	return r.Store.QueueWithPhotos(m, parts, photos, nil)
}

// Handle commits the update before doing work. Authorized commands use the
// same path as prompts, and provisioning consumes the same concurrency slot.
// All progress logs here are console-only; user-visible replies go through reply().
//
// At-most-once: Accept persists before execution so a crash after Accept loses
// both the reply and its memory source (redelivery is dropped as duplicate).
// Moving Accept after CommitExchange would risk duplicate tool side effects,
// so memory inherits this window. In-process CommitExchange retries stay
// idempotent via LastSourceID.
func (r *Runtime) Handle(ctx context.Context, m platform.Message) error {
	accepted, err := r.Store.Accept(m.UpdateID)
	if err != nil {
		r.logf("[TELEGRAM] Update acceptance failed | update=%d error=%q", m.UpdateID, r.Options.Redaction.Text(redaction.Terminal, err.Error()))
		return err
	}
	if !accepted {
		r.logf("[TELEGRAM] Duplicate update skipped | update=%d", m.UpdateID)
		return nil
	}
	if !r.allowed(m) {
		r.logf("[TELEGRAM] Message rejected | update=%d chat=%d topic=%d user=%d reason=%q", m.UpdateID, m.ChatID, m.TopicID, m.UserID, "not approved")
		return nil
	}
	r.logf("[TELEGRAM] Message received | update=%d chat=%d topic=%d user=%d", m.UpdateID, m.ChatID, m.TopicID, m.UserID)
	if err := r.Store.RecordChat(m, time.Now()); err != nil {
		r.logf("[TELEGRAM] Chat recording failed | update=%d error=%q", m.UpdateID, r.Options.Redaction.Text(redaction.Terminal, err.Error()))
	}
	if m.CallbackID != "" {
		return r.handleCallback(ctx, m)
	}
	if fields := strings.Fields(m.Text); len(fields) > 0 && (fields[0] == "/update" || fields[0] == "/restart") {
		return r.maintenanceCommand(ctx, m, fields)
	}
	if fields := strings.Fields(m.Text); len(fields) > 0 && (fields[0] == "/provider" || fields[0] == "/model" || fields[0] == "/think" || fields[0] == "/max_steps" || fields[0] == "/settings") {
		return r.chatSettings(m, fields)
	}
	if fields := strings.Fields(m.Text); len(fields) > 0 && fields[0] == "/companion" {
		return r.companionCommand(m, fields)
	}
	if m.Text == "/models" {
		return r.showModels(ctx, m)
	}
	// Only authorized, accepted messages may cause a Telegram file download.
	if len(m.PhotoIDs) > 0 && !strings.HasPrefix(m.Text, "/") {
		loader, ok := r.Sink.(interface {
			DownloadPhoto(context.Context, string) (llm.Image, error)
		})
		if !ok {
			return r.reply(m, "Image input is unavailable in this adapter.")
		}
		if len(m.PhotoIDs) > 2 {
			return r.reply(m, "Too many attached images (maximum 2 per message).")
		}
		for _, id := range m.PhotoIDs {
			image, err := loader.DownloadPhoto(ctx, id)
			if err != nil {
				return r.reply(m, "Could not load the attached image: "+err.Error()+". Please resend it.")
			}
			m.Images = append(m.Images, image)
		}
		m.PhotoIDs = nil
		m.Text += "\n\n[Attached image(s) are available for this task only; resend them if needed in a later task.]"
	}
	// Generic files (documents, video, audio, voice, animation) are saved to
	// the chat workspace inbox before busy routing so follow-ups keep them.
	if len(m.Files) > 0 && !strings.HasPrefix(m.Text, "/") {
		if err := r.saveFiles(ctx, &m); err != nil {
			return r.reply(m, err.Error()+". Please resend it.")
		}
	} else {
		m.Files = nil
	}
	r.mu.Lock()
	if r.active == nil {
		r.active = map[string]task{}
	}
	if r.shellPrompts == nil {
		r.shellPrompts = map[string]*shellDecision{}
	}
	if r.closed {
		r.mu.Unlock()
		return context.Canceled
	}
	key := m.WorkspaceKey()
	active, busy := r.active[key]
	// A pending shell-timeout prompt owns this conversation until resolved.
	// Allow status/help through; /cancel resolves as kill then cancels.
	if dec, ok := r.shellPrompts[key]; ok && dec.owner.UserID == m.UserID && dec.owner.TopicID == m.TopicID {
		if m.Text == "/cancel" {
			delete(r.shellPrompts, key)
			select {
			case dec.ch <- false:
			default:
			}
			// Fall through to normal cancellation below.
		} else if m.Text == "/status" || m.Text == "/help" || m.Text == "/memory" || m.Text == "/forget" {
			// Let the switch handle these without consuming the prompt.
		} else if extend, isDecision := parseShellDecision(m.Text); isDecision {
			delete(r.shellPrompts, key)
			select {
			case dec.ch <- extend:
			default:
			}
			ack := "Shell termination requested."
			if extend {
				ack = "Shell continuing for another window."
			}
			r.mu.Unlock()
			r.logf("[SHELL] Timeout decision received | update=%d extend=%t", m.UpdateID, extend)
			return r.reply(m, ack)
		} else {
			r.mu.Unlock()
			return r.reply(m, "Shell is waiting: reply `extend` to continue or `kill` to stop it.")
		}
		active, busy = r.active[key]
	}
	if m.Text == "/help" {
		text := helpText()
		r.mu.Unlock()
		return r.reply(m, text)
	}
	switch m.Text {
	case "/status":
		text := r.statusLocked(m)
		r.mu.Unlock()
		return r.reply(m, text)
	case "/help":
		r.mu.Unlock()
		return r.reply(m, "**q-agent commands**\n`/update` — Install the latest stable Release and restart (private admin only).\n`/restart` — Restart the systemd user service (private admin only).\n`/status` — Show current runtime, process RSS memory, and safe settings (no model call).\n`/models` — Choose a model for this chat.\n`/provider` — Show providers or choose this chat's provider (`/provider <name|default>` also works).\n`/model` <id|default> — Set this chat’s model.\n`/think` <level|auto|default> — Set this chat’s thinking.\n`/max_steps` [number|default] — Show or set this chat’s model-step limit.\n`/settings` [reset] — Show settings or restore defaults.\n`/memory` — Show this chat’s long-term memory.\n`/forget` — Clear notes and pending memory sources across all topics; running tasks may add new sources when they finish.\n`/help` — Show this help.\n`/new` — Clear this topic's conversation; keep workspace.\n`/cancel` — Cancel this topic's active task.\n\nSend a message to start a task. Send follow-up text in the same topic to steer your running task. `/cancel` stops the task and pending follow-ups.\n\nIn groups, address commands to @your_bot (e.g. `/status@your_bot`), mention the bot, or reply to it.")
	case "/memory":
		text := r.memoryShow(m)
		r.mu.Unlock()
		return r.reply(m, text)
	case "/forget":
		r.mu.Unlock()
		return r.forgetMemory(m)
	case "/cancel":
		if busy && active.topic == m.TopicID {
			active.userCancelled = true
			r.active[key] = active
			active.cancel()
			r.logf("[RUN] Cancellation requested | update=%d", m.UpdateID)
			r.mu.Unlock()
			return r.reply(m, "Cancellation requested.")
		}
		r.logf("[RUN] Cancellation skipped; no active task | update=%d", m.UpdateID)
		r.mu.Unlock()
		return r.reply(m, "No active task in this conversation.")
	case "/new":
		if busy && active.topic == m.TopicID {
			r.logf("[RUN] Conversation reset rejected; task active | update=%d", m.UpdateID)
			r.mu.Unlock()
			return r.reply(m, "This conversation is busy. Use /cancel before /new.")
		}
		err := r.Store.Reset(m.ConversationKey())
		r.mu.Unlock()
		if err != nil {
			r.logf("[RUN] Conversation reset failed | update=%d error=%q", m.UpdateID, r.Options.Redaction.Text(redaction.Terminal, err.Error()))
			return err
		}
		r.logf("[RUN] Conversation cleared | update=%d", m.UpdateID)
		return r.reply(m, "Conversation cleared. Workspace and pending memory sources retained.")
	}
	// Keep command and shell-decision parsing on the user's actual text. Fold
	// reply context once here so new tasks, steers, and late follow-ups all
	// retain it through the ordinary prompt/history path.
	m = withReplyContext(m)
	limit := r.Config.Limits.MaxConcurrent
	if r.Maintenance != nil && r.Maintenance.Busy() {
		r.mu.Unlock()
		return r.reply(m, "Service maintenance is in progress. Try again after startup completes.")
	}
	if limit <= 0 {
		limit = 2
	}
	if busy && active.topic == m.TopicID && active.user == m.UserID {
		err := r.steerLocked(m, active)
		r.mu.Unlock()
		return err
	}
	if busy || len(r.active) >= limit {
		r.logf("[RUN] Task rejected; capacity busy | update=%d workspace_busy=%t active_tasks=%d max_tasks=%d", m.UpdateID, busy, len(r.active), limit)
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
	r.logf("[RUN] Task started | update=%d chat=%d topic=%d", m.UpdateID, m.ChatID, m.TopicID)
	live := newLiveReporter(r.Sink, m, r.Options.Redaction)
	// Only announce provisioning if it is slow (real first-time setup).
	// The fast path (existing chat folder, e.g. right after /models)
	// skips straight to contacting the model to avoid confusion.
	preparingTimer := time.AfterFunc(800*time.Millisecond, func() { live.SetStatus("Working\u2026 preparing workspace") })
	answer := ""
	var runErr error
	var continuationSaved bool
	var continuationSaveErr error
	root, err := r.Workspaces.Ensure(ctx, m)
	preparingTimer.Stop()
	if err != nil {
		runErr = fmt.Errorf("workspace provisioning: %w", err)
		r.logf("[RUN] Workspace preparation failed | update=%d elapsed=%s error=%q", m.UpdateID, time.Since(runStart).Truncate(time.Millisecond), r.Options.Redaction.Text(redaction.Terminal, runErr.Error()))
	} else {
		conversation, err := r.loadModelConversation(m)
		if err != nil {
			runErr = err
			r.logf("[RUN] Conversation loading failed | update=%d elapsed=%s error=%q", m.UpdateID, time.Since(runStart).Truncate(time.Millisecond), r.Options.Redaction.Text(redaction.Terminal, err.Error()))
		} else {
			r.logf("[MODEL] Model selected | update=%d model=%q", m.UpdateID, conversation.Model)
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
					r.logf("[RUN] Provider identity saving failed | update=%d elapsed=%s error=%q", m.UpdateID, time.Since(runStart).Truncate(time.Millisecond), r.Options.Redaction.Text(redaction.Terminal, err.Error()))
				} else {
					options := r.Options
					options.Model = conversation.Model
					options.Thinking = conversation.Thinking
					options.MaxSteps = conversation.MaxSteps
					options.Images = m.Images
					options.NextInput = nil
					options.NextMessages = func(final bool) []llm.Message { return r.takeSteer(m.WorkspaceKey(), final) }
					options.OnSteer = live.Steered
					live.SetStatus("Working\u2026 contacting " + conversation.Model)
					toolCount := 0
					options.OnStream = func(ev llm.StreamEvent) { live.StreamEvent(ev) }
					options.OnToolStart = func(name string) {
						toolCount++
						live.ToolStart(name, toolCount)
					}
					options.OnToolEnd = func(name string, toolErr error) { live.ToolEnd(name, toolCount, toolErr) }
					baseTools := r.Tools(root)
					if setter, ok := baseTools.(interface{ SetProcessHooks(process.Hooks) }); ok {
						interimSequence := 0
						r.mu.Lock()
						active := r.active[m.WorkspaceKey()]
						r.mu.Unlock()
						setter.SetProcessHooks(process.Hooks{
							OnStart: active.interactive, Wake: active.wake, HumanAvailable: runID == "",
							HasInput: func() bool { r.mu.Lock(); defer r.mu.Unlock(); return len(r.active[m.WorkspaceKey()].pending) > 0 },
							Interim: func(text string) error {
								interimSequence++
								return r.Store.QueueTaskNotice(m, runID, interimSequence, telegram.FormatAndSplit(r.Options.Redaction.Text(redaction.Remote, text)))
							},
						})
					}
					if setter, ok := baseTools.(shellHookSetter); ok {
						hookOwner := m
						hookRunID := runID
						setter.SetShellTimeoutHook(func(hookCtx context.Context, command string, lastLines []string) bool {
							return r.requestShellExtend(hookCtx, hookOwner, hookRunID, command, lastLines)
						}, r.Config.ShellExtends())
					}
					// A cancelled run must not leave a shell prompt parked.
					defer func() {
						r.mu.Lock()
						if dec, ok := r.shellPrompts[m.WorkspaceKey()]; ok && dec.owner.UpdateID == m.UpdateID {
							delete(r.shellPrompts, m.WorkspaceKey())
							select {
							case dec.ch <- false:
							default:
							}
						}
						r.mu.Unlock()
					}()
					result, err := agent.Run(ctx, provider, &scheduleTools{base: baseTools, runtime: r, owner: m}, r.memoryHistory(m, conversation), m.Text, options)
					answer = result.Answer
					runErr = err
					if err == nil {
						r.logf("[RUN] Agent result received | update=%d elapsed=%s steps=%d tool_calls=%d", m.UpdateID, time.Since(runStart).Truncate(time.Millisecond), result.ModelSteps, result.ToolCalls)
						conversation.Pairs = append(conversation.Pairs, state.Pair{User: m.Text, Answer: answer, Steers: result.Steers})
						sourceID := fmt.Sprintf("update:%s:%d", m.Platform, m.UpdateID)
						if runID != "" {
							sourceID = "schedule:" + runID
						}
						if r.Config.Memory.Enabled {
							err = r.Store.CommitExchange(m, sourceID, conversation, time.Now())
						} else {
							err = r.Store.Save(m.ConversationKey(), conversation)
						}
						if err != nil {
							runErr = fmt.Errorf("save history: %w", err)
							r.logf("[RUN] History saving failed | update=%d elapsed=%s error=%q", m.UpdateID, time.Since(runStart).Truncate(time.Millisecond), r.Options.Redaction.Text(redaction.Terminal, err.Error()))
						}
					} else {
						r.logf("[RUN] Agent stopped with error | update=%d elapsed=%s error=%q", m.UpdateID, time.Since(runStart).Truncate(time.Millisecond), r.Options.Redaction.Text(redaction.Terminal, err.Error()))
						// Explicit cancellation does not add the cancelled run to history.
						// Other failures retain bounded context, not a replayable tool queue.
						if ctx.Err() != context.Canceled {
							failure := "Task interrupted without a final answer: " + r.Options.Redaction.Text(redaction.Remote, err.Error())
							conversation.Pairs = append(conversation.Pairs, state.Pair{User: m.Text, Answer: failure, Steers: result.Steers, Progress: result.Progress, Interrupted: true})
							if saveErr := r.Store.Save(m.ConversationKey(), conversation); saveErr != nil {
								continuationSaveErr = saveErr
								r.report(fmt.Errorf("save continuation context: %w", saveErr))
							} else {
								continuationSaved = true
							}
						}
					}
				}
			}
		}
	}
	if runErr != nil {
		outcome := "failed"
		if ctx.Err() == context.Canceled {
			answer = "Task cancelled."
			outcome = "cancelled"
		} else if ctx.Err() == context.DeadlineExceeded {
			answer = "Task reached its deadline."
			outcome = "timed out"
		} else {
			answer = "Task failed: " + r.Options.Redaction.Text(redaction.Remote, runErr.Error())
		}
		r.report(runErr)
		if continuationSaved {
			answer += "\nTask context saved. Send a follow-up in this topic to continue."
		} else if continuationSaveErr != nil {
			answer += "\nWarning: task context could not be saved; please restate the original request and verify any completed actions."
		}
		if pending := r.discardSteer(m.WorkspaceKey()); pending > 0 {
			answer += fmt.Sprintf("\n%d pending follow-up message(s) were not applied; please resend them.", pending)
		}
		r.logf("[RUN] Task %s | update=%d duration=%s error=%q", outcome, m.UpdateID, time.Since(runStart).Truncate(time.Millisecond), r.Options.Redaction.Text(redaction.Terminal, runErr.Error()))
	} else {
		r.logf("[RUN] Task execution completed | update=%d duration=%s", m.UpdateID, time.Since(runStart).Truncate(time.Millisecond))
	}
	if runErr != nil {
		live.FinishError(answer)
	} else {
		live.FinishSuccess()
	}
	var replyErr error
	if runID == "" {
		replyErr = r.replyAnswer(m, answer, root)
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
		photos, cleaned, _ := telegram.ExtractPhotos(answer, root)
		var parts []string
		if len(photos) > 0 && len(strings.TrimSpace(cleaned)) == 0 {
			parts = nil
		} else {
			parts = telegram.FormatAndSplit(cleaned)
		}
		if len(photos) == 0 {
			replyErr = r.Store.FinishRun(runID, status, parts)
		} else {
			replyErr = r.Store.FinishRunWithPhotos(runID, status, parts, photos)
		}
	}
	if err := replyErr; err != nil {
		r.logf("[TELEGRAM] Reply queueing failed | update=%d elapsed=%s error=%q", m.UpdateID, time.Since(runStart).Truncate(time.Millisecond), r.Options.Redaction.Text(redaction.Terminal, err.Error()))
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
	if r.digestCancel != nil {
		r.digestCancel()
	}
	if r.proactiveCancel != nil {
		r.proactiveCancel()
	}
	for _, t := range r.active {
		t.cancel()
	}
	done := r.scheduleDone
	digestDone := r.digestDone
	proactiveDone := r.proactiveDone
	r.mu.Unlock()
	if done != nil {
		<-done
	}
	if digestDone != nil {
		<-digestDone
	}
	if proactiveDone != nil {
		<-proactiveDone
	}
	r.wg.Wait()
}
func (r *Runtime) Wait() { r.wg.Wait() }
