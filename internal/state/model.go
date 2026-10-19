package state

import "path/filepath"

// ChatModel stores chat-wide overrides separately from topic history so /new
// retains preferences. Empty Model inherits the selected provider's model.
// ProviderPinned distinguishes an explicit provider from legacy model-only
// selections, which continue following the global provider. MaxSteps zero
// inherits the global limit. A nil Thinking inherits configured defaults; a
// pointer to "" requests the provider default.
type ChatModel struct {
	// Zero inherits the global per-task model-step limit.
	MaxSteps       int     `json:"max_steps,omitempty"`
	ProviderPinned bool    `json:"provider_pinned,omitempty"`
	Thinking       *string `json:"thinking,omitempty"`
	Revision       string  `json:"revision,omitempty"`
	Provider       string  `json:"provider"`
	Model          string  `json:"model"`
}

func (s *Store) ChatModel(key string) (ChatModel, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var model ChatModel
	err := read(filepath.Join(s.dir, "models", key+".json"), &model)
	return model, err
}
func (s *Store) SetChatModel(key string, model ChatModel) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Atomic(filepath.Join(s.dir, "models", key+".json"), model)
}
