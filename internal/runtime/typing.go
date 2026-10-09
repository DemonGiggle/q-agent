package runtime

import (
	"context"
	"q-agent/internal/platform"
	"time"
)

func startTyping(ctx context.Context, sink Sink, m platform.Message) func() {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(4 * time.Second)
		defer ticker.Stop()
		for {
			requestCtx, stop := context.WithTimeout(ctx, 5*time.Second)
			_ = sink.Typing(requestCtx, m)
			stop()
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return func() { cancel(); <-done }
}
