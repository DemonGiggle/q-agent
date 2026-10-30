//go:build !linux

package sandbox

import (
	"context"
	"fmt"
	"time"

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
func PutInboxFile(context.Context, Options, string, string, []byte, int64) (string, error) {
	return "", fmt.Errorf("sandbox requires Linux")
}
func ClearInbox(context.Context, Options, string, time.Time) (int, int64, error) {
	return 0, 0, fmt.Errorf("sandbox requires Linux")
}
