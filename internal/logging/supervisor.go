package logging

import (
	"fmt"
	"io"
	"os"
	"os/exec"
)

// Run supervises a detached process and captures both streams in a rotating
// log. The supervisor persists across midnight while its child is running.
func Run(path string, argv []string) error {
	if len(argv) == 0 {
		return fmt.Errorf("missing supervised command")
	}
	w := &Writer{Path: path}
	if err := w.Prepare(); err != nil {
		return err
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		return err
	}
	defer reader.Close()
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin = nil
	cmd.Stdout, cmd.Stderr = writer, writer
	if err := cmd.Start(); err != nil {
		writer.Close()
		_, _ = fmt.Fprintf(w, "could not start detached process: %v\n", err)
		return err
	}
	writer.Close()
	var copyErr error
	buffer := make([]byte, 32*1024)
	for {
		n, readErr := reader.Read(buffer)
		if n > 0 {
			if _, err := w.Write(buffer[:n]); err != nil && copyErr == nil {
				copyErr = err
			}
		}
		if readErr != nil {
			if readErr != io.EOF && copyErr == nil {
				copyErr = readErr
			}
			break
		}
	}
	waitErr := cmd.Wait()
	if copyErr != nil {
		return copyErr
	}
	return waitErr
}
