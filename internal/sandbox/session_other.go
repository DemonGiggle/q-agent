//go:build !linux

package sandbox

import (
	"context"
	"fmt"
	"io"
	"q-agent/internal/process"
)

func sessionLauncher(Options, string) process.Launcher {
	return func(context.Context, process.Spec, io.Writer) (process.Child, error) {
		return nil, fmt.Errorf("sandbox requires Linux")
	}
}
