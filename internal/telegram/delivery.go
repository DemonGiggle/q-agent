package telegram

import (
	"context"
	"errors"
	"q-agent/internal/state"
	"time"
)

// Deliver drains completed replies, resuming from the last saved chunk after
// restart. Network ambiguity can duplicate a chunk; agent work never reruns.
func (c *Client) Deliver(ctx context.Context, store *state.Store, report func(error)) {
	delay := 250 * time.Millisecond
	for {
		reply, ok := store.Pending()
		if ok {
			requestCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			var err error
			if reply.Keyboard != nil {
				err = c.SendKeyboard(requestCtx, reply.Route, reply.Parts[reply.Next], reply.Keyboard)
			} else {
				err = c.Send(requestCtx, reply.Route, reply.Parts[reply.Next])
			}
			cancel()
			if err == nil {
				if err = store.Delivered(reply.ID); err == nil {
					delay = 250 * time.Millisecond
					continue
				}
			}
			if report != nil {
				report(err)
			}
			if deferErr := store.DeferReply(reply.ID); deferErr != nil && report != nil {
				report(deferErr)
			}
			delay *= 2
			if delay > 30*time.Second {
				delay = 30 * time.Second
			}
			var api *APIError
			if errors.As(err, &api) && api.RetryAfter > 0 {
				delay = time.Duration(api.RetryAfter) * time.Second
			}
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}
