package telegram

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf16"
	"unicode/utf8"
)

func TestNormalizeReplyContext(t *testing.T) {
	bot := User{ID: 99, IsBot: true, Username: "our_bot"}
	for _, tc := range []struct {
		name, kind, text, reply string
		wantText                string
		wantSelf, wantAccepted  bool
	}{
		{"private bot reply", "private", "tell me more", `{"from":{"id":99,"is_bot":true},"text":"The riot began after an election."}`, "The riot began after an election.", true, true},
		{"group bot reply", "supergroup", "tell me more", `{"from":{"id":99,"is_bot":true},"text":"The riot began after an election.","message_thread_id":8}`, "The riot began after an election.", true, true},
		{"photo caption", "private", "explain this", `{"from":{"id":99,"is_bot":true},"caption":"A map of the riot in 台北 🙂"}`, "A map of the riot in 台北 🙂", true, true},
		{"blank text uses caption", "private", "explain this", `{"from":{"id":99},"text":"  ","caption":"Map caption"}`, "Map caption", true, true},
		{"text preferred", "private", "explain this", `{"from":{"id":99},"text":"Original @our_bot text","caption":"Other text"}`, "Original @our_bot text", true, true},
		{"mentioned human reply", "group", "@our_bot explain this", `{"from":{"id":2},"text":"A participant's message"}`, "A participant's message", false, true},
		{"other bot reply", "private", "explain this", `{"from":{"id":88,"is_bot":true},"text":"Another bot's message"}`, "Another bot's message", false, true},
		{"unknown sender", "private", "explain this", `{"text":"Original message"}`, "Original message", false, true},
		{"unaddressed human reply", "group", "explain this", `{"from":{"id":2},"text":"Not addressed to q-agent"}`, "", false, false},
		{"no reply content", "private", "explain this", `{"from":{"id":99}}`, "", false, true},
		{"only immediate reply", "private", "explain this", `{"from":{"id":99},"text":"Immediate message","reply_to_message":{"text":"Nested message"}}`, "Immediate message", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data, err := json.Marshal(map[string]any{
				"update_id": 3,
				"message": map[string]any{
					"message_id": 4, "message_thread_id": 7, "from": User{ID: 1},
					"chat": Chat{ID: -10, Type: tc.kind}, "text": tc.text,
					"reply_to_message": json.RawMessage(tc.reply),
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			var update Update
			if err := json.Unmarshal(data, &update); err != nil {
				t.Fatal(err)
			}
			out, ok := Normalize(update, bot)
			if ok != tc.wantAccepted {
				t.Fatalf("accepted=%t, want %t", ok, tc.wantAccepted)
			}
			if !ok {
				return
			}
			if out.Text != strings.TrimSpace(strings.TrimPrefix(tc.text, "@our_bot")) || out.ChatID != -10 || out.TopicID != 7 || out.MessageID != 4 || out.UpdateID != 3 {
				t.Fatalf("reply changed user text or routing: %+v", out)
			}
			if tc.wantText == "" {
				if out.ReplyTo != nil {
					t.Fatalf("empty reply context: %+v", out.ReplyTo)
				}
			} else if out.ReplyTo == nil || out.ReplyTo.Text != tc.wantText || out.ReplyTo.FromSelf != tc.wantSelf {
				t.Fatalf("reply context: %+v", out.ReplyTo)
			}
		})
	}
}

func TestAddressing(t *testing.T) {
	bot := User{ID: 99, Username: "our_bot"}
	for _, tc := range []struct {
		name, kind, text string
		reply            bool
		want             bool
		normalized       string
	}{
		{"private max steps", "private", "/max_steps 20", false, true, "/max_steps 20"},
		{"targeted max steps", "supergroup", "/max_steps@OUR_BOT 20", false, true, "/max_steps 20"},
		{"mentioned max steps", "group", "/max_steps default @our_bot", false, true, "/max_steps default"},
		{"bare max steps", "private", "/max_steps", false, true, "/max_steps"},
		{"unaddressed max steps", "group", "/max_steps 20", false, false, ""},
		{"wrong max steps target", "private", "/max_steps@other 20", false, false, ""},
		{"private provider", "private", "/provider openai", false, true, "/provider openai"},
		{"targeted model", "group", "/model@OUR_BOT remote", false, true, "/model remote"},
		{"mentioned think", "supergroup", "/think high @our_bot", false, true, "/think high"},
		{"reset settings", "group", "/settings@our_bot reset", false, true, "/settings reset"},
		{"unaddressed provider", "group", "/provider openai", false, false, ""},
		{"wrong think target", "private", "/think@other high", false, false, ""},
		{"private restart", "private", "/restart", false, true, "/restart"},
		{"targeted update", "private", "/update@OUR_BOT", false, true, "/update"},
		{"preserve restart arguments", "private", "/restart now", false, true, "/restart now"},
		{"mentioned group update reaches authorization", "supergroup", "/update @our_bot", false, true, "/update"},
		{"unaddressed restart", "group", "/restart", false, false, ""},
		{"wrong update target", "private", "/update@other", false, false, ""},
		{"wrong restart target", "private", "/restart@other", false, false, ""},
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
		{"private update", "private", "/update", false, true, "/update"},
		{"targeted restart", "group", "/restart@our_bot", false, true, "/restart"},
		{"update args preserved", "private", "/update --arch arm64", false, true, "/update --arch arm64"},
		{"unaddressed update", "group", "/update", false, false, ""},
		{"private", "private", "hello", false, true, "hello"}, {"group ignored", "group", "hello", false, false, ""}, {"mention", "supergroup", "hello @our_bot", false, true, "hello"}, {"wrong mention", "group", "@our_bot2 hi", false, false, ""}, {"ask removed", "group", "/ask@our_bot do this", false, false, ""}, {"ask mention removed", "group", "/ask do this @our_bot", false, false, ""}, {"ask private removed", "private", "/ask hi", false, false, ""}, {"reply", "group", "hello", true, true, "hello"}, {"new", "group", "/new@our_bot", false, true, "/new"}, {"new mention", "group", "/new @our_bot", false, true, "/new"}, {"unaddressed cancel", "group", "/cancel", false, false, ""}, {"private cancel", "private", "/cancel", false, true, "/cancel"},
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

func TestModelsAddressingAndCallbackNormalization(t *testing.T) {
	bot := User{ID: 99, Username: "bot"}
	for _, tc := range []struct {
		kind, text string
		want       bool
	}{
		{"private", "/models", true}, {"supergroup", "/models", false},
		{"supergroup", "/models@bot", true}, {"supergroup", "/models@other", false},
		{"supergroup", "/models @bot", true},
	} {
		m, ok := Normalize(Update{ID: 1, Message: &Message{ID: 2, Chat: Chat{ID: 10, Type: tc.kind}, From: &User{ID: 1}, Text: tc.text}}, bot)
		if ok != tc.want || ok && m.Text != "/models" {
			t.Errorf("%+v: %+v %v", tc, m, ok)
		}
	}
	for _, tc := range []struct {
		name    string
		from    *User
		message *Message
		data    string
		want    bool
	}{
		{"valid", &User{ID: 1}, &Message{ID: 2, Chat: Chat{ID: 10, Type: "private"}, From: &bot}, "models:abc:p:0", true},
		{"bot click", &User{ID: 1, IsBot: true}, &Message{From: &bot}, "models:abc:p:0", false},
		{"inline message", &User{ID: 1}, nil, "models:abc:p:0", false},
		{"other bot", &User{ID: 1}, &Message{From: &User{ID: 88}}, "models:abc:p:0", false},
		{"other callback", &User{ID: 1}, &Message{From: &bot}, "other", false},
	} {
		_, ok := Normalize(Update{ID: 1, Callback: &CallbackQuery{ID: "q", From: tc.from, Message: tc.message, Data: tc.data}}, bot)
		if ok != tc.want {
			t.Errorf("%s: %v", tc.name, ok)
		}
	}
}
