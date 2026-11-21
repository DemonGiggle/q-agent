// Package learning defines the host-side contracts for autonomous research.
package learning

import (
	"context"
	"errors"
	"time"
)

var ErrDenied = errors.New("learning scope denied")

type Visibility string

const (
	PrivateChat Visibility = "private_chat"
	GroupChat   Visibility = "group_chat"
)

// Scope is a concrete platform chat and topic. A nil TopicID requests the
// whole chat, subject to authorization. A pointer to zero requests topic 0.
type Scope struct {
	Platform   string     `json:"platform"`
	ChatID     int64      `json:"chat_id"`
	TopicID    *int64     `json:"topic_id,omitempty"`
	Visibility Visibility `json:"visibility"`
}

type ConversationQuery struct {
	Scope Scope
	From  time.Time // inclusive
	Until time.Time // exclusive
}

type ConversationTurn struct {
	OriginRef  string
	TopicID    int64
	At         time.Time
	Transcript string
}

type ConversationReader interface {
	Recent(context.Context, ConversationQuery) ([]ConversationTurn, error)
}
