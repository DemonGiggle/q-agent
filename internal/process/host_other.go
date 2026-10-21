//go:build !linux

package process

import (
	"context"
	"fmt"
	"io"
)

func HostLauncher(string, []string) Launcher {
	return func(context.Context, Spec, io.Writer) (Child, error) {
		return nil, fmt.Errorf("process sessions require Linux")
	}
}
