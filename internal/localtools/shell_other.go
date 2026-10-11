//go:build !linux

package localtools

import (
	"context"
	"fmt"
)

func runShell(context.Context, string, Options) (string, error) {
	return "", fmt.Errorf("tool execution requires Linux")
}
