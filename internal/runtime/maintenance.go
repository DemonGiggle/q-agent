package runtime

import (
	"context"
	"fmt"
	"strings"

	"q-agent/internal/platform"
)

func (r *Runtime) maintenanceCommand(ctx context.Context, m platform.Message, fields []string) error {
	admin := false
	for _, id := range r.Config.Telegram.AdminUsers {
		admin = admin || id == m.UserID
	}
	if !admin || m.ChatID != m.UserID || m.TopicID != 0 {
		return r.reply(m, "Service administration requires a private chat with a configured telegram.admin_user_ids administrator.")
	}
	if len(fields) != 1 {
		return r.reply(m, "Usage: "+fields[0]+" (no arguments).")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return context.Canceled
	}
	if len(r.active) != 0 {
		return r.reply(m, "Tasks are still running. Wait for all tasks to finish, or cancel them before maintenance.")
	}
	if r.Maintenance == nil {
		return r.reply(m, "Systemd service maintenance is not available in this runtime.")
	}
	if err := r.Maintenance.Request(ctx, strings.TrimPrefix(fields[0], "/"), m); err != nil {
		return r.reply(m, fmt.Sprintf("Maintenance was not started: %s", err))
	}
	return nil
}
