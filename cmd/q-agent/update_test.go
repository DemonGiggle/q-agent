package main

import (
	"io"
	"os"
	"strings"
	"testing"
)

func TestUpdateCLI(t *testing.T) {
	originalArgs, originalOut, originalErr := os.Args, os.Stdout, os.Stderr
	originalVersion := version
	t.Cleanup(func() {
		os.Args, os.Stdout, os.Stderr = originalArgs, originalOut, originalErr
		version = originalVersion
	})
	t.Setenv("HOME", t.TempDir()) // No configuration or credentials required.
	version = "v1.2.3"
	for _, tc := range []struct {
		args    []string
		want    string
		wantErr bool
	}{
		{args: []string{"version"}, want: "v1.2.3"},
		{args: []string{"--version"}, want: "v1.2.3"},
		{args: []string{"update", "--help"}, want: "Usage: q-agent update"},
		{args: []string{"update", "--arch", "386"}, wantErr: true},
		{args: []string{"update", "--unknown"}, wantErr: true},
		{args: []string{"update", "extra"}, wantErr: true},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			output, err := os.CreateTemp(t.TempDir(), "output")
			if err != nil {
				t.Fatal(err)
			}
			defer output.Close()
			os.Stdout, os.Stderr = output, output
			os.Args = append([]string{"q-agent"}, tc.args...)
			err = mainRun()
			if (err != nil) != tc.wantErr {
				t.Fatalf("error = %v, wantErr = %v", err, tc.wantErr)
			}
			if _, err := output.Seek(0, io.SeekStart); err != nil {
				t.Fatal(err)
			}
			data, err := io.ReadAll(output)
			if err != nil || !strings.Contains(string(data), tc.want) {
				t.Fatalf("output = %q, error = %v; want %q", data, err, tc.want)
			}
		})
	}
}
