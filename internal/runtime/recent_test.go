package runtime

import (
	"context"
	"strings"
	"testing"
	"time"

	"q-agent/internal/platform"
	"q-agent/internal/state"
)

func tapInput(updateID, chat, topic, user int64, text string) platform.Message {
	return platform.Message{Platform: "telegram", ChatID: chat, TopicID: topic, UserID: user, MessageID: updateID, UpdateID: updateID, Text: text}
}

func TestObserveTapRules(t *testing.T) {
	r, _, _ := fixture(t)
	now := time.Now()
	r.Config.Proactive.ActiveTime = now.Add(-time.Hour).Format("15:04") + "-" + now.Add(time.Hour).Format("15:04")
	group := tapInput(1, -10, 0, 2, "group chatter without mention")
	r.ObserveTapMessage(group, group.Text, now)
	msgs, err := r.Store.RecentSince(group.WorkspaceKey(), now.Add(-time.Hour))
	must(t, err)
	if len(msgs) != 1 {
		t.Fatalf("group tap not stored: %d", len(msgs))
	}
	// Private taps are skipped (history already complete).
	priv := tapInput(2, 1, 0, 1, "private hi")
	r.ObserveTapMessage(priv, priv.Text, now)
	msgs, _ = r.Store.RecentSince(priv.WorkspaceKey(), now.Add(-time.Hour))
	if len(msgs) != 0 {
		t.Fatal("private tap should be skipped")
	}
	// Kill switch stops collection.
	r.Config.Proactive.Enabled = false
	r.ObserveTapMessage(tapInput(3, -10, 0, 3, "after off"), "after off", now)
	msgs, _ = r.Store.RecentSince(group.WorkspaceKey(), now.Add(-time.Hour))
	if len(msgs) != 1 {
		t.Fatal("kill switch tap stored")
	}
	r.Config.Proactive.Enabled = true
	// Group policy off skips collection.
	r.Config.Proactive.GroupPolicy = "off"
	r.ObserveTapMessage(tapInput(4, -10, 0, 4, "group off"), "group off", now)
	msgs, _ = r.Store.RecentSince(group.WorkspaceKey(), now.Add(-time.Hour))
	if len(msgs) != 1 {
		t.Fatal("group-policy-off tap stored")
	}
	r.Config.Proactive.GroupPolicy = "conservative"
}

func TestProactiveInputIncludesRecentAndConsumes(t *testing.T) {
	r, _, _ := fixture(t)
	now := time.Now()
	r.Config.Proactive.ActiveTime = now.Add(-time.Hour).Format("15:04") + "-" + now.Add(time.Hour).Format("15:04")
	// Tap-only group: no addressed history, but sweep must see taps.
	groupRoute := tapInput(10, -10, 0, 2, "any")
	must(t, r.Store.AppendRecent(groupRoute, now.Add(-time.Hour), "deployment at 3pm tomorrow", 200, 32768))
	chats, err := r.Store.ListChats()
	must(t, err)
	var chat state.ChatRecordWithKey
	for _, c := range chats {
		if c.Key == groupRoute.WorkspaceKey() {
			chat = c
		}
	}
	if chat.Key == "" {
		t.Fatal("tap-only chat missing")
	}
	input, ok := r.buildProactiveInput(chat)
	if !ok || !strings.Contains(input, "deployment at 3pm") || !strings.Contains(input, "Live chat") {
		t.Fatalf("recent missing: %q %v", input, ok)
	}
	// Processed sweep consumes up to watermark; taps after watermark stay.
	watermark := now.Add(-45 * time.Minute)
	must(t, r.Store.AppendRecent(tapInput(11, -10, 0, 3, "second"), now.Add(-30*time.Minute), "second tap", 200, 32768))
	r.consumeRecentProcessed(chat.Key, watermark)
	msgs, _ := r.Store.RecentSince(chat.Key, now.Add(-12*time.Hour))
	if len(msgs) != 1 || msgs[0].Text != "second tap" {
		t.Fatalf("consume kept %+v", msgs)
	}
	// Expiry prune removes old taps even when unprocessed.
	must(t, r.Store.AppendRecent(groupRoute, now.Add(-13*time.Hour), "expired", 200, 32768))
	_ = r.Store.ConsumeRecent(chat.Key, now.Add(-12*time.Hour))
	msgs, _ = r.Store.RecentSince(chat.Key, now.Add(-12*time.Hour))
	for _, m := range msgs {
		if m.Text == "expired" {
			t.Fatal("expired not pruned")
		}
	}
}

func TestProactiveSilentConsumesRecent(t *testing.T) {
	r, p, chat := companionFixture(t)
	now := time.Now()
	route := chat.WorkspaceRoute()
	tap := tapInput(99, route.ChatID, 0, 9, "group chatter raises trigger")
	must(t, r.Store.AppendRecent(tap, now.Add(-time.Minute), tap.Text, 200, 32768))
	p.verdict = `{"verdict":"silent","reason":"not needed"}`
	must(t, r.proactiveChat(context.Background(), chat))
	msgs, _ := r.Store.RecentSince(chat.Key, now.Add(-12*time.Hour))
	if len(msgs) != 0 {
		t.Fatalf("silent must consume taps, kept %d", len(msgs))
	}
}
