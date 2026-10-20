package runtime

import (
	"fmt"
	"strings"

	"q-agent/internal/platform"
	"q-agent/internal/trust"
)

func withReplyContext(m platform.Message) platform.Message {
	reply := m.ReplyTo
	m.ReplyTo = nil
	if reply == nil || strings.TrimSpace(reply.Text) == "" {
		return m
	}
	sender := "another participant"
	if reply.FromSelf {
		sender = "q-agent"
	}
	m.Text = fmt.Sprintf("The user is replying to this earlier Telegram message from %s. Use it as context for their reply:\n%s\n\nUser reply:\n%s",
		sender, trust.Wrap("telegram_reply", reply.Text), m.Text)
	return m
}
