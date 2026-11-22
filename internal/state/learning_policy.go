package state

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"q-agent/internal/learning"
)

func LoadLearningPolicy(stateDir string) (learning.Config, error) {
	file, err := os.Open(filepath.Join(stateDir, "learning", "policy.json"))
	if err != nil {
		return learning.Config{}, err
	}
	defer file.Close()
	return ReadLearningPolicy(file)
}

func ReadLearningPolicy(input io.Reader) (learning.Config, error) {
	const maxSize = 64 << 10
	data, err := io.ReadAll(io.LimitReader(input, maxSize+1))
	if err != nil {
		return learning.Config{}, err
	}
	if len(data) > maxSize {
		return learning.Config{}, fmt.Errorf("learning configuration exceeds 64 KiB")
	}
	cfg := learning.DefaultConfig()
	policy := &cfg
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&policy); err != nil {
		return learning.Config{}, fmt.Errorf("invalid learning configuration: %w", err)
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return learning.Config{}, fmt.Errorf("learning configuration must contain one JSON object")
	}
	if policy == nil {
		return learning.Config{}, fmt.Errorf("learning configuration requires an object")
	}
	return cfg, cfg.Validate()
}
