package telegram

import (
	"strings"
	"testing"
	"unicode/utf16"
	"unicode/utf8"
)

func TestAddressing(t *testing.T) {
	bot := User{ID: 99, Username: "our_bot"}
	for _, tc := range []struct {
		name, kind, text string
		reply            bool
		want             bool
		normalized       string
	}{
		{"private status", "private", "/status", false, true, "/status"},
		{"targeted status", "group", "/status@OUR_BOT", false, true, "/status"},
		{"mentioned status", "supergroup", "/status @our_bot", false, true, "/status"},
		{"reply status", "group", "/status", true, true, "/status"},
		{"unaddressed status", "group", "/status", false, false, ""},
		{"wrong status target", "private", "/status@other", false, false, ""},
		{"private help", "private", "/help", false, true, "/help"},
		{"targeted help", "group", "/help@our_bot", false, true, "/help"},
		{"unaddressed help", "group", "/help", false, false, ""},
		{"wrong help target", "private", "/help@other", false, false, ""},
		{"private", "private", "hello", false, true, "hello"}, {"group ignored", "group", "hello", false, false, ""}, {"mention", "supergroup", "hello @our_bot", false, true, "hello"}, {"wrong mention", "group", "@our_bot2 hi", false, false, ""}, {"ask", "group", "/ask@our_bot do this", false, true, "do this"}, {"ask mention", "group", "/ask do this @our_bot", false, true, "do this"}, {"wrong target", "private", "/ask@other hi", false, false, ""}, {"reply", "group", "hello", true, true, "hello"}, {"new", "group", "/new@our_bot", false, true, "/new"}, {"new mention", "group", "/new @our_bot", false, true, "/new"}, {"unaddressed cancel", "group", "/cancel", false, false, ""}, {"private cancel", "private", "/cancel", false, true, "/cancel"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &Message{From: &User{ID: 1}, Chat: Chat{ID: -5, Type: tc.kind}, Text: tc.text, TopicID: 7}
			if tc.reply {
				m.Reply = &Message{From: &bot}
			}
			out, ok := Normalize(Update{ID: 3, Message: m}, bot)
			if ok != tc.want || ok && (out.Text != tc.normalized || out.TopicID != 7 || out.UserID != 1) {
				t.Fatalf("%+v %v", out, ok)
			}
		})
	}
}
func TestUTF16Mention(t *testing.T) {
	m := &Message{From: &User{ID: 1}, Chat: Chat{ID: -5, Type: "group"}, Text: "😀 @OUR_BOT, go", Entities: []Entity{{Type: "mention", Offset: 3, Length: 8}}}
	out, ok := Normalize(Update{Message: m}, User{ID: 99, Username: "our_bot"})
	if !ok || out.Text != "😀 , go" {
		t.Fatalf("%+v %v", out, ok)
	}
}
func TestSplit(t *testing.T) {
	text := strings.Repeat("漢😀é", 3000)
	parts := Split(text)
	if strings.Join(parts, "") != text {
		t.Fatal("text changed")
	}
	for _, part := range parts {
		if !utf8.ValidString(part) || len(utf16.Encode([]rune(part))) > 4096 {
			t.Fatal("invalid split")
		}
	}
}
