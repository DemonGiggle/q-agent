//go:build linux

package sandbox

import (
	"context"
	"fmt"
	"io"
	"os"
	"sync"

	"q-agent/internal/process"
)

func sessionLauncher(o Options, rootfs string) process.Launcher {
	return func(ctx context.Context, spec process.Spec, output io.Writer) (process.Child, error) {
		inputR, inputW, err := os.Pipe()
		if err != nil {
			return nil, err
		}
		defer inputR.Close()
		outputR, outputW, err := os.Pipe()
		if err != nil {
			inputW.Close()
			return nil, err
		}
		defer outputW.Close()
		worker, err := startWorker(ctx, o, toolMounts(o, rootfs, "session"), inputR, outputW)
		inputR.Close()
		outputW.Close()
		if err != nil {
			inputW.Close()
			outputR.Close()
			return nil, FatalError{err}
		}
		c := &sandboxChild{worker: worker, input: inputW, output: outputR, writer: frameWriter{w: inputW}, done: make(chan struct{}), ack: make(chan streamFrame, 1), stopped: make(chan struct{})}
		fail := func(err error) (process.Child, error) { c.Stop(); _ = worker.wait(); return nil, FatalError{err} }
		if err = c.writer.send(streamFrame{Type: "start", Spec: &spec}); err != nil {
			return fail(err)
		}
		ready, err := readFrame(outputR)
		if err != nil {
			return fail(fmt.Errorf("session handshake: %w", err))
		}
		if ready.Type != "ready" {
			return fail(fmt.Errorf("invalid session handshake"))
		}
		if ready.Error != "" {
			c.Stop()
			_ = worker.wait()
			return nil, fmt.Errorf("%s", ready.Error)
		}
		go func() {
			exit := process.Exit{Code: -1}
			for {
				f, err := readFrame(outputR)
				if err != nil {
					exit.Err = FatalError{fmt.Errorf("session protocol: %w", err)}
					break
				}
				switch f.Type {
				case "output":
					if _, err = output.Write(f.Data); err != nil {
						exit.Err = FatalError{err}
					}
				case "result":
					select {
					case c.ack <- f:
					default:
						exit.Err = FatalError{fmt.Errorf("unexpected control response")}
					}
				case "exit":
					exit.Code = f.Code
				default:
					exit.Err = FatalError{fmt.Errorf("unknown session event")}
				}
				if f.Type == "exit" || exit.Err != nil {
					break
				}
			}
			if exit.Err != nil {
				worker.stop()
			}
			boundaryErr := worker.wait()
			select {
			case <-c.stopped:
				exit.Err = nil
			default:
				if ctx.Err() == nil && boundaryErr != nil && exit.Err == nil {
					exit.Err = FatalError{boundaryErr}
				}
			}
			inputW.Close()
			outputR.Close()
			c.exit = exit
			close(c.done)
		}()
		return c, nil
	}
}

type sandboxChild struct {
	worker  *workerProcess
	input   *os.File
	output  *os.File
	writer  frameWriter
	mu      sync.Mutex
	once    sync.Once
	done    chan struct{}
	stopped chan struct{}
	ack     chan streamFrame
	exit    process.Exit
}

func (c *sandboxChild) request(f streamFrame) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	select {
	case <-c.done:
		if c.exit.Err != nil {
			return c.exit.Err
		}
		return fmt.Errorf("session has exited")
	default:
	}
	if err := c.writer.send(f); err != nil {
		return FatalError{fmt.Errorf("session control: %w", err)}
	}
	select {
	case response := <-c.ack:
		if response.Error != "" {
			return fmt.Errorf("%s", response.Error)
		}
		return nil
	case <-c.done:
		if c.exit.Err != nil {
			return c.exit.Err
		}
		return fmt.Errorf("session has exited")
	}
}
func (c *sandboxChild) Write(p []byte) (int, error) {
	err := c.request(streamFrame{Type: "write", Data: p})
	if err != nil {
		return 0, err
	}
	return len(p), nil
}
func (c *sandboxChild) Resize(rows, cols int) error {
	return c.request(streamFrame{Type: "resize", Rows: rows, Columns: cols})
}
func (c *sandboxChild) Stop() {
	c.once.Do(func() { close(c.stopped); c.worker.stop(); c.input.Close(); c.output.Close() })
}
func (c *sandboxChild) Wait() process.Exit { <-c.done; return c.exit }
