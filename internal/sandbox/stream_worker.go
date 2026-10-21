package sandbox

import (
	"context"
	"fmt"
	"io"
	"os"
	"sync"

	"q-agent/internal/process"
)

func streamWorker(in io.Reader, out io.Writer) error {
	first, err := readFrame(in)
	if err != nil {
		return err
	}
	if first.Type != "start" || first.Spec == nil {
		return fmt.Errorf("expected session start frame")
	}
	spec := *first.Spec
	if spec.Rows < 1 || spec.Rows > 120 || spec.Columns < 2 || spec.Columns > 240 {
		return fmt.Errorf("invalid terminal dimensions")
	}
	writer := &frameWriter{w: out}
	// Hold output until the ready frame, including commands with immediate output.
	var gate sync.Mutex
	gate.Lock()
	child, err := process.HostLauncher("/workspace", os.Environ())(context.Background(), spec, streamOutput{writer: writer, gate: &gate})
	if err != nil {
		gate.Unlock()
		return writer.send(streamFrame{Type: "ready", Error: err.Error()})
	}
	defer func() { child.Stop(); _ = child.Wait() }()
	if err = writer.send(streamFrame{Type: "ready"}); err != nil {
		gate.Unlock()
		return err
	}
	gate.Unlock()
	exited := make(chan process.Exit, 1)
	go func() { exited <- child.Wait() }()
	requests := make(chan streamFrame)
	readErrors := make(chan error, 1)
	done := make(chan struct{})
	defer close(done)
	// Return on exit even if the host has not sent another control request.
	go func() {
		for {
			f, err := readFrame(in)
			if err != nil {
				readErrors <- err
				return
			}
			select {
			case requests <- f:
			case <-done:
				return
			}
		}
	}()
	for {
		select {
		case exit := <-exited:
			return writer.send(streamFrame{Type: "exit", Code: exit.Code})
		case err := <-readErrors:
			if err == io.EOF {
				return nil
			}
			return err
		case f := <-requests:
			var appErr error
			switch f.Type {
			case "write":
				if len(f.Data) > 64<<10 {
					return fmt.Errorf("oversized input frame")
				}
				_, appErr = child.Write(f.Data)
			case "resize":
				if !spec.PTY || f.Rows < 1 || f.Rows > 120 || f.Columns < 2 || f.Columns > 240 {
					return fmt.Errorf("invalid resize frame")
				}
				appErr = child.Resize(f.Rows, f.Columns)
			case "terminate":
				child.Stop()
			default:
				return fmt.Errorf("unknown control frame")
			}
			response := streamFrame{Type: "result"}
			if appErr != nil {
				response.Error = appErr.Error()
			}
			if err = writer.send(response); err != nil {
				return err
			}
		}
	}
}

type streamOutput struct {
	writer *frameWriter
	gate   *sync.Mutex
}

func (w streamOutput) Write(p []byte) (int, error) {
	w.gate.Lock()
	defer w.gate.Unlock()
	total := len(p)
	for len(p) > 0 {
		n := min(len(p), 16<<10)
		if err := w.writer.send(streamFrame{Type: "output", Data: p[:n]}); err != nil {
			return 0, err
		}
		p = p[n:]
	}
	return total, nil
}
