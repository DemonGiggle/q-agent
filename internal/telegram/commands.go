package telegram

import (
	"context"
)

// BotCommand is a Telegram Bot API bot command without the leading slash.
type BotCommand struct {
	Command     string `json:"command"`
	Description string `json:"description"`
}

// DefaultCommands is the canonical q-agent command menu, in the same order
// as /help and README. Telegram requires lowercase commands.
func DefaultCommands() []BotCommand {
	return []BotCommand{
		{Command: "update", Description: "Install latest Release and restart (admin only)"},
		{Command: "restart", Description: "Restart service (admin only)"},
		{Command: "status", Description: "Show runtime status"},
		{Command: "models", Description: "Choose a model for this chat"},
		{Command: "provider", Description: "Choose this chat's provider"},
		{Command: "model", Description: "Set this chat's model"},
		{Command: "think", Description: "Set this chat's thinking level"},
		{Command: "max_steps", Description: "Show or set this chat's maximum model steps"},
		{Command: "settings", Description: "Show settings or restore defaults"},
		{Command: "memory", Description: "Show notes, pending sources, retries, and memory loss"},
		{Command: "forget", Description: "Clear notes and pending memory sources across all topics"},
		{Command: "help", Description: "Show command list"},
		{Command: "new", Description: "Clear topic conversation, keep workspace"},
		{Command: "cancel", Description: "Cancel active task"},
	}
}

func equalCommands(a, b []BotCommand) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Command != b[i].Command || a[i].Description != b[i].Description {
			return false
		}
	}
	return true
}

// GetCommands returns the bot's current default-scope command menu.
func (c *Client) GetCommands(ctx context.Context) ([]BotCommand, error) {
	var commands []BotCommand
	if err := c.call(ctx, "getMyCommands", map[string]any{}, &commands); err != nil {
		return nil, err
	}
	if commands == nil {
		commands = []BotCommand{}
	}
	return commands, nil
}

// SetCommands replaces the bot's default-scope command menu.
func (c *Client) SetCommands(ctx context.Context, commands []BotCommand) error {
	if commands == nil {
		commands = []BotCommand{}
	}
	return c.call(ctx, "setMyCommands", map[string]any{"commands": commands}, nil)
}

// SyncCommands overwrites a stale menu left by a previous bot implementation.
// It is idempotent: no setMyCommands call when the server already serves
// DefaultCommands in order.
func (c *Client) SyncCommands(ctx context.Context) error {
	current, err := c.GetCommands(ctx)
	if err != nil {
		return err
	}
	want := DefaultCommands()
	if equalCommands(current, want) {
		return nil
	}
	return c.SetCommands(ctx, want)
}
