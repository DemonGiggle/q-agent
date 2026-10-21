package platform

import (
	"crypto/sha256"
	"fmt"
	"q-agent/internal/llm"
)

// Message is the normalized adapter boundary. IDs preserve their platform value.
type Message struct {
	// Input-only: never persist Telegram file IDs or binary image data in routes.
	PhotoIDs     []string    `json:"-"`
	Images       []llm.Image `json:"-"`
	Platform     string      `json:"platform"`
	ChatID       int64       `json:"chat_id"`
	TopicID      int64       `json:"topic_id,omitempty"`
	UserID       int64       `json:"user_id"`
	MessageID    int64       `json:"message_id"`
	UpdateID     int64       `json:"update_id"`
	Text         string      `json:"text"`
	CallbackID   string      `json:"callback_id,omitempty"`
	CallbackData string      `json:"callback_data,omitempty"`
	// ReplyTo is input-only context. The runtime folds it into the user prompt
	// before saving history; delivery and schedule routes must not persist it.
	ReplyTo *ReplyContext `json:"-"`
}

type ReplyContext struct {
	Text     string
	FromSelf bool
}

func WorkspaceKey(platform string, chatID int64) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprintf("%s:%d", platform, chatID))))
}
func (m Message) WorkspaceKey() string    { return WorkspaceKey(m.Platform, m.ChatID) }
func (m Message) ConversationKey() string { return fmt.Sprintf("%s:%d", m.WorkspaceKey(), m.TopicID) }

// InlineKeyboard contains only callback buttons; callback data must fit 64 bytes.
type InlineButton struct {
	Text string `json:"text"`
	Data string `json:"callback_data"`
}
type InlineKeyboard struct {
	Rows [][]InlineButton `json:"inline_keyboard"`
}
