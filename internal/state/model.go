package state

import "path/filepath"

// ChatModel is separate from topic history so /new retains the chat preference.
type ChatModel struct {
	Revision string `json:"revision,omitempty"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
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
