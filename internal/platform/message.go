package platform

import (
	"crypto/sha256"
	"fmt"
)

// Message is the normalized adapter boundary. IDs preserve their platform value.
type Message struct {
	Platform  string `json:"platform"`
	ChatID    int64  `json:"chat_id"`
	TopicID   int64  `json:"topic_id,omitempty"`
	UserID    int64  `json:"user_id"`
	MessageID int64  `json:"message_id"`
	UpdateID  int64  `json:"update_id"`
	Text      string `json:"text"`
}

func WorkspaceKey(platform string, chatID int64) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprintf("%s:%d", platform, chatID))))
}
func (m Message) WorkspaceKey() string    { return WorkspaceKey(m.Platform, m.ChatID) }
func (m Message) ConversationKey() string { return fmt.Sprintf("%s:%d", m.WorkspaceKey(), m.TopicID) }
