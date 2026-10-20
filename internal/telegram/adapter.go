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
type PhotoSize struct {
	FileID   string `json:"file_id"`
	Width    int    `json:"width"`
	Height   int    `json:"height"`
	FileSize int64  `json:"file_size,omitempty"`
}

type Message struct {
	Photo           []PhotoSize `json:"photo,omitempty"`
	CaptionEntities []Entity    `json:"caption_entities,omitempty"`
	ID              int64       `json:"message_id"`
	TopicID         int64       `json:"message_thread_id,omitempty"`
	From            *User       `json:"from"`
	Chat            Chat        `json:"chat"`
	Text            string      `json:"text"`
	Caption         string      `json:"caption,omitempty"`
	Entities        []Entity    `json:"entities"`
	Reply           *Message    `json:"reply_to_message,omitempty"`
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
	if m == nil || m.From == nil || m.From.IsBot {
		return platform.Message{}, false
	}
	private := m.Chat.Type == "private"
	if !private && m.Chat.Type != "group" && m.Chat.Type != "supergroup" {
		return platform.Message{}, false
	}
	raw, entities := m.Text, m.Entities
	if strings.TrimSpace(raw) == "" {
		raw, entities = m.Caption, m.CaptionEntities
	}
	text := strings.TrimSpace(raw)
	first := ""
	if fields := strings.Fields(text); len(fields) > 0 {
		first = fields[0]
	}
	addressed := private || m.Reply != nil && m.Reply.From != nil && m.Reply.From.ID == bot.ID
	mention := "@" + bot.Username
	for _, entity := range entities {
		value := entityText(raw, entity)
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
	photos := photoIDs(m)
	if m.Reply != nil {
		photos = append(photos, photoIDs(m.Reply)...)
	}
	if !addressed || text == "" && len(photos) == 0 {
		return platform.Message{}, false
	}
	if text == "" {
		text = "Describe the attached image."
	}
	// /new and /cancel with a mention in place of a command target.
	if strings.HasPrefix(text, "/new") || strings.HasPrefix(text, "/cancel") {
		parts := strings.Fields(text)
		if parts[0] == "/new" || parts[0] == "/cancel" {
			text = parts[0]
		}
	}
	out := platform.Message{Platform: "telegram", ChatID: m.Chat.ID, TopicID: m.TopicID, UserID: m.From.ID, MessageID: m.ID, UpdateID: update.ID, Text: text, PhotoIDs: photos}
	if reply := m.Reply; reply != nil {
		replyText := reply.Text
		if strings.TrimSpace(replyText) == "" {
			replyText = reply.Caption
		}
		if strings.TrimSpace(replyText) != "" {
			out.ReplyTo = &platform.ReplyContext{Text: replyText, FromSelf: reply.From != nil && reply.From.ID == bot.ID}
		}
	}
	return out, true
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

// Telegram photo contains multiple sizes of the same image, not separate images.
func photoIDs(m *Message) []string {
	var best *PhotoSize
	for i := range m.Photo {
		p := &m.Photo[i]
		if p.FileID != "" && (best == nil || int64(p.Width)*int64(p.Height) > int64(best.Width)*int64(best.Height)) {
			best = p
		}
	}
	if best == nil {
		return nil
	}
	return []string{best.FileID}
}
