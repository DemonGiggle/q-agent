//go:build !linux

package localtools

import (
	"context"
	"fmt"
	"time"
)

func runShell(context.Context, string, time.Duration) (string, error) {
	return "", fmt.Errorf("tool worker requires Linux")
}
