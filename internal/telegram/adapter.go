package telegram

import (
	"q-agent/internal/platform"
	"strings"
	"unicode/utf16"
)

type User struct {
	ID       int64  `json:"id"`
	IsBot    bool   `json:"is_bot"`
	Username string `json:"username"`
}
type Chat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
}
type Entity struct {
	Type   string `json:"type"`
	Offset int    `json:"offset"`
	Length int    `json:"length"`
	User   *User  `json:"user,omitempty"`
}
type Message struct {
	ID       int64    `json:"message_id"`
	TopicID  int64    `json:"message_thread_id,omitempty"`
	From     *User    `json:"from"`
	Chat     Chat     `json:"chat"`
	Text     string   `json:"text"`
	Entities []Entity `json:"entities"`
	Reply    *Message `json:"reply_to_message,omitempty"`
}
type CallbackQuery struct {
	ID      string   `json:"id"`
	From    *User    `json:"from"`
	Message *Message `json:"message"`
	Data    string   `json:"data"`
}
type Update struct {
	Callback *CallbackQuery `json:"callback_query,omitempty"`
	ID       int64          `json:"update_id"`
	Message  *Message       `json:"message,omitempty"`
}

func entityText(text string, e Entity) string {
	u := utf16.Encode([]rune(text))
	if e.Offset < 0 || e.Length < 0 || e.Offset > len(u) || e.Length > len(u)-e.Offset {
		return ""
	}
	return string(utf16.Decode(u[e.Offset : e.Offset+e.Length]))
}

// Normalize filters group addressing after Telegram privacy mode is disabled.
func Normalize(update Update, bot User) (platform.Message, bool) {
	if q := update.Callback; q != nil {
		if q.ID == "" || q.From == nil || q.From.IsBot || q.Message == nil || q.Message.From == nil || q.Message.From.ID != bot.ID || !strings.HasPrefix(q.Data, "models:") {
			return platform.Message{}, false
		}
		m := q.Message
		if m.Chat.Type != "private" && m.Chat.Type != "group" && m.Chat.Type != "supergroup" {
			return platform.Message{}, false
		}
		return platform.Message{Platform: "telegram", ChatID: m.Chat.ID, TopicID: m.TopicID, UserID: q.From.ID, MessageID: m.ID, UpdateID: update.ID, CallbackID: q.ID, CallbackData: q.Data}, true
	}
	m := update.Message
	if m == nil || m.From == nil || m.From.IsBot || strings.TrimSpace(m.Text) == "" {
		return platform.Message{}, false
	}
	private := m.Chat.Type == "private"
	if !private && m.Chat.Type != "group" && m.Chat.Type != "supergroup" {
		return platform.Message{}, false
	}
	text := strings.TrimSpace(m.Text)
	fields := strings.Fields(text)
	first := fields[0]
	addressed := private || m.Reply != nil && m.Reply.From != nil && m.Reply.From.ID == bot.ID
	mention := "@" + bot.Username
	for _, entity := range m.Entities {
		value := entityText(m.Text, entity)
		if entity.Type == "mention" && strings.EqualFold(value, mention) || entity.Type == "text_mention" && entity.User != nil && entity.User.ID == bot.ID {
			addressed = true
			if entity.Type == "mention" {
				text = strings.ReplaceAll(text, value, "")
			}
		}
	}
	// Accept bare @name tokens too, useful for clients omitting entities. Do not
	// match another username, punctuation suffix, or email substring.
	for _, word := range strings.Fields(text) {
		if strings.EqualFold(word, mention) {
			addressed = true
			text = strings.ReplaceAll(text, word, "")
		}
	}
	// Commands explicitly addressed to another bot are always ignored.
	if strings.HasPrefix(first, "/") {
		command, target, hasTarget := strings.Cut(first, "@")
		if hasTarget && !strings.EqualFold(target, bot.Username) {
			return platform.Message{}, false
		}
		if hasTarget {
			addressed = true
		}
		switch command {
		case "/ask":
			if !addressed {
				return platform.Message{}, false
			}
			text = strings.TrimSpace(strings.TrimPrefix(text, first))
		case "/provider", "/model", "/think", "/max_steps", "/settings":
			if !addressed {
				return platform.Message{}, false
			}
			text = command + " " + strings.TrimSpace(strings.TrimPrefix(text, first))
		case "/update", "/restart":
			if !addressed {
				return platform.Message{}, false
			}
			text = command + " " + strings.TrimSpace(strings.TrimPrefix(text, first))
		case "/new", "/cancel", "/status", "/help", "/models", "/memory", "/forget":
			if !addressed {
				return platform.Message{}, false
			}
			text = command
		default:
			return platform.Message{}, false
		}
	}
	text = strings.TrimSpace(text)
	if !addressed || text == "" {
		return platform.Message{}, false
	}
	// /new and /cancel with a mention in place of a command target.
	if strings.HasPrefix(text, "/new") || strings.HasPrefix(text, "/cancel") {
		parts := strings.Fields(text)
		if parts[0] == "/new" || parts[0] == "/cancel" {
			text = parts[0]
		}
	}
	return platform.Message{Platform: "telegram", ChatID: m.Chat.ID, TopicID: m.TopicID, UserID: m.From.ID, MessageID: m.ID, UpdateID: update.ID, Text: text}, true
}

// Split uses UTF-16 code units, a conservative interpretation of Telegram's
// 4096-character limit that also protects astral Unicode characters.
func Split(text string) []string {
	if text == "" {
		text = "(No answer text returned.)"
	}
	var parts []string
	start, count := 0, 0
	for i, r := range text {
		width := 1
		if r > 0xffff {
			width = 2
		}
		if count+width > 4096 {
			parts = append(parts, text[start:i])
			start = i
			count = 0
		}
		count += width
	}
	return append(parts, text[start:])
}
