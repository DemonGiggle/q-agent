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
			isForum := tc.kind == "supergroup"
			data, err := json.Marshal(map[string]any{
				"update_id": 3,
				"message": map[string]any{
					"message_id": 4, "message_thread_id": 7, "is_topic_message": isForum, "from": User{ID: 1},
					"chat": Chat{ID: -10, Type: tc.kind, IsForum: isForum}, "text": tc.text,
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
			wantTopic := int64(0)
			if isForum {
				wantTopic = 7
			}
			if out.Text != strings.TrimSpace(strings.TrimPrefix(tc.text, "@our_bot")) || out.ChatID != -10 || out.TopicID != wantTopic || out.MessageID != 4 || out.UpdateID != 3 {
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
			isForum := tc.kind == "supergroup"
			m := &Message{From: &User{ID: 1}, Chat: Chat{ID: -5, Type: tc.kind, IsForum: isForum}, Text: tc.text, TopicID: 7, IsTopicMessage: isForum}
			if tc.reply {
				m.Reply = &Message{From: &bot}
			}
			out, ok := Normalize(Update{ID: 3, Message: m}, bot)
			wantTopic := int64(0)
			if isForum {
				wantTopic = 7
			}
			if ok != tc.want || ok && (out.Text != tc.normalized || out.TopicID != wantTopic || out.UserID != 1) {
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

func TestNormalizeIncomingPhotos(t *testing.T) {
	bot := User{ID: 99, Username: "our_bot"}
	for _, tc := range []struct {
		name, raw string
		accepted  bool
		ids       int
	}{
		{"caption mention", `{"chat":{"id":-10,"type":"group"},"caption":"😀 @OUR_BOT 看看","caption_entities":[{"type":"mention","offset":3,"length":8}],"photo":[{"file_id":"small","width":10,"height":10},{"file_id":"large","width":100,"height":100}]}`, true, 1},
		{"mention only", `{"chat":{"type":"group"},"caption":"@our_bot","photo":[{"file_id":"a"}]}`, true, 1},
		{"private photo", `{"chat":{"type":"private"},"photo":[{"file_id":"a"}]}`, true, 1},
		{"unaddressed photo", `{"chat":{"type":"group"},"photo":[{"file_id":"a"}]}`, false, 0},
		{"reply photo with mention", `{"chat":{"type":"group"},"text":"@our_bot describe","reply_to_message":{"photo":[{"file_id":"a"}]}}`, true, 1},
		{"reply photo without mention", `{"chat":{"type":"group"},"text":"describe","reply_to_message":{"from":{"id":2},"photo":[{"file_id":"a"}]}}`, false, 0},
		{"other bot", `{"chat":{"type":"group"},"caption":"/help@other","photo":[{"file_id":"a"}]}`, false, 0},
		{"empty", `{"chat":{"type":"private"}}`, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var m Message
			if err := json.Unmarshal([]byte(tc.raw), &m); err != nil {
				t.Fatal(err)
			}
			m.From = &User{ID: 1}
			out, ok := Normalize(Update{Message: &m}, bot)
			if ok != tc.accepted || len(out.PhotoIDs) != tc.ids {
				t.Fatalf("ok=%v out=%+v", ok, out)
			}
			if ok && strings.TrimSpace(out.Text) == "" {
				t.Fatal("missing default prompt")
			}
			if tc.name == "caption mention" && (out.PhotoIDs[0] != "large" || strings.Contains(out.Text, "OUR_BOT")) {
				t.Fatalf("bad photo/mention: %+v", out)
			}
		})
	}
}

func TestNormalizeTopicIDRequiresForumTopicMessage(t *testing.T) {
	bot := User{ID: 99, Username: "our_bot"}
	for _, tc := range []struct {
		name          string
		chat          Chat
		topicID       int64
		isTopic       bool
		wantTopicID   int64
		wantNonForum  bool
	}{
		{"forum topic message keeps topic", Chat{ID: -100, Type: "supergroup", IsForum: true}, 42, true, 42, false},
		{"forum general message without is_topic_message drops thread id", Chat{ID: -100, Type: "supergroup", IsForum: true}, 42, false, 0, false},
		{"non-forum supergroup drops legacy thread id", Chat{ID: -100, Type: "supergroup", IsForum: false}, 42, false, 0, true},
		{"non-forum supergroup even with is_topic_message drops thread id", Chat{ID: -100, Type: "supergroup", IsForum: false}, 42, true, 0, true},
		{"regular group drops thread id", Chat{ID: -10, Type: "group"}, 42, false, 0, true},
		{"private chat drops thread id and is not non-forum group", Chat{ID: 1, Type: "private"}, 42, false, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			msg := &Message{
				ID:             5,
				From:           &User{ID: 1},
				Chat:           tc.chat,
				TopicID:        tc.topicID,
				IsTopicMessage: tc.isTopic,
				Text:           "@our_bot hello",
			}
			out, ok := Normalize(Update{ID: 10, Message: msg}, bot)
			if !ok {
				t.Fatal("expected normalized message")
			}
			if out.TopicID != tc.wantTopicID || out.NonForumGroup != tc.wantNonForum {
				t.Fatalf("Normalize got topic=%d nonForum=%t, want topic=%d nonForum=%t", out.TopicID, out.NonForumGroup, tc.wantTopicID, tc.wantNonForum)
			}
			tap, _, tapOK := ExtractTap(Update{ID: 10, Message: msg})
			if !tapOK {
				t.Fatal("expected tap message")
			}
			if tap.TopicID != tc.wantTopicID || tap.NonForumGroup != tc.wantNonForum {
				t.Fatalf("ExtractTap got topic=%d nonForum=%t, want topic=%d nonForum=%t", tap.TopicID, tap.NonForumGroup, tc.wantTopicID, tc.wantNonForum)
			}
			cbMsg := &Message{
				ID:             6,
				From:           &bot,
				Chat:           tc.chat,
				TopicID:        tc.topicID,
				IsTopicMessage: tc.isTopic,
			}
			cbOut, cbOK := Normalize(Update{ID: 11, Callback: &CallbackQuery{ID: "q", From: &User{ID: 1}, Message: cbMsg, Data: "models:tok:p:0"}}, bot)
			if !cbOK {
				t.Fatal("expected normalized callback")
			}
			if cbOut.TopicID != tc.wantTopicID || cbOut.NonForumGroup != tc.wantNonForum {
				t.Fatalf("Callback got topic=%d nonForum=%t, want topic=%d nonForum=%t", cbOut.TopicID, cbOut.NonForumGroup, tc.wantTopicID, tc.wantNonForum)
			}
		})
	}
}
