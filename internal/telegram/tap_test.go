package telegram

import (
	"testing"
)

func TestExtractTap(t *testing.T) {
	for _, tc := range []struct {
		name string
		msg  *Message
		want bool
		text string
	}{
		{"group chatter", &Message{From: &User{ID: 1}, Chat: Chat{ID: -10, Type: "supergroup"}, Text: "hello everyone"}, true, "hello everyone"},
		{"group other-bot command kept", &Message{From: &User{ID: 1}, Chat: Chat{ID: -10, Type: "group"}, Text: "/start@otherbot"}, true, "/start@otherbot"},
		{"private kept", &Message{From: &User{ID: 1}, Chat: Chat{ID: 1, Type: "private"}, Text: "hi"}, true, "hi"},
		{"bot ignored", &Message{From: &User{ID: 99, IsBot: true}, Chat: Chat{ID: -10, Type: "group"}, Text: "hi"}, false, ""},
		{"empty ignored", &Message{From: &User{ID: 1}, Chat: Chat{ID: -10, Type: "group"}, Text: "  "}, false, ""},
		{"photo placeholder", &Message{From: &User{ID: 1}, Chat: Chat{ID: -10, Type: "group"}, Text: "  ", Photo: []PhotoSize{{FileID: "f", Width: 10, Height: 10}}}, true, "[photo]"},
		{"caption kept", &Message{From: &User{ID: 1}, Chat: Chat{ID: -10, Type: "group"}, Caption: "look"}, true, "look"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, text, ok := ExtractTap(Update{ID: 7, Message: tc.msg})
			if ok != tc.want {
				t.Fatalf("ok=%t want %t", ok, tc.want)
			}
			if !ok {
				return
			}
			if text != tc.text || out.Text != tc.text || out.ChatID != tc.msg.Chat.ID || out.UserID != 1 || out.UpdateID != 7 {
				t.Fatalf("tap=%+v text=%q", out, text)
			}
		})
	}
	// Callbacks never tap.
	if _, _, ok := ExtractTap(Update{Callback: &CallbackQuery{ID: "q"}}); ok {
		t.Fatal("callback tapped")
	}
	// Reply folds context.
	m := &Message{From: &User{ID: 1}, Chat: Chat{ID: -10, Type: "group"}, Text: "agreed", Reply: &Message{From: &User{ID: 2}, Text: "proposal"}}
	_, text, ok := ExtractTap(Update{ID: 8, Message: m})
	if !ok || text == "agreed" {
		t.Fatalf("reply not folded: %q", text)
	}
}
