package runtime

import (
	"context"

	"q-agent/internal/learning"
	"q-agent/internal/platform"
)

// learningConversationReader binds access to a concrete authorized origin.
// Enabling group learning requires a separate opt-in from chat authorization.
func (r *Runtime) learningConversationReader(origin platform.Message, policy learning.PermissionConfig, excluded func(learning.Scope, learning.ConversationTurn) bool) learning.ConversationReader {
	return r.Store.NewLearningReader(func(_ context.Context, scope learning.Scope) bool {
		return r.Config.Memory.Enabled && r.allowed(origin) && scope.Platform == origin.Platform &&
			scope.ChatID == origin.ChatID && scope.TopicID != nil && *scope.TopicID == origin.TopicID &&
			(policy.IncludePrivateChats || scope.Visibility != learning.PrivateChat) &&
			(policy.IncludeGroupChats || scope.Visibility != learning.GroupChat)
	}, func(scope learning.Scope, turn learning.ConversationTurn) bool {
		return policy.Excludes(turn) || (excluded != nil && excluded(scope, turn))
	})
}
