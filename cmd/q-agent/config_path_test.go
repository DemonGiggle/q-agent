package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRuntimeConfigPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	originalArgs := os.Args
	t.Cleanup(func() { os.Args = originalArgs })
	for _, command := range [][]string{{"serve"}, {"image", "build"}} {
		for _, override := range []bool{false, true} {
			args := append([]string{"q-agent"}, command...)
			want := filepath.Join(home, ".config", "q-agent", "config.toml")
			if override {
				want = filepath.Join(home, "custom.toml")
				args = append(args, "--config", want)
			}
			t.Run(strings.Join(args[1:], " "), func(t *testing.T) {
				os.Args = args
				err := mainRun()
				var pathErr *os.PathError
				if !errors.As(err, &pathErr) || !errors.Is(err, os.ErrNotExist) || pathErr.Path != want {
					t.Fatalf("error = %v; want missing config %s", err, want)
				}
			})
		}
	}
}
