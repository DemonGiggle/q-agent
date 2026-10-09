//go:build !linux

package sandbox

import (
	"context"
	"fmt"
	"q-agent/internal/llm"
)

func Resolve(o Options) (Options, error) { return o, fmt.Errorf("sandbox requires Linux") }
func execute(context.Context, Options, string, llm.ToolCall) (string, error) {
	return "", fmt.Errorf("sandbox requires Linux")
}
func Provision(context.Context, Options, string, string) error {
	return fmt.Errorf("sandbox requires Linux")
}
func Remove(context.Context, Options, string) error { return fmt.Errorf("sandbox requires Linux") }
