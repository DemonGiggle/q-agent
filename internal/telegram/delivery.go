package telegram

import (
	"context"
	"errors"
	"os"
	"strings"
	"time"

	"q-agent/internal/state"
)

// Deliver drains completed replies, resuming from the last saved chunk after
// restart. Photos go first in Reply.Next order, then text parts. Permanent
// photo failures fall back to the preformatted link/text so the chat never
// goes silent. Network ambiguity can duplicate a chunk; agent work never reruns.
func (c *Client) Deliver(ctx context.Context, store *state.Store, report func(error)) {
	delay := 250 * time.Millisecond
	for {
		reply, ok := store.Pending()
		if ok {
			requestCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			var err error
			if reply.Next < len(reply.Photos) {
				ph := reply.Photos[reply.Next]
				err = c.SendPhoto(requestCtx, reply.Route, ph.LocalPath, ph.RemoteURL, ph.Caption)
				if err != nil && isPermanentPhotoError(err) {
					fallback := ph.Fallback
					if fallback == "" {
						fallback = "(Image could not be posted as a photo.)"
					}
					if ferr := c.Send(requestCtx, reply.Route, fallback); ferr == nil {
						cancel()
						if derr := store.Delivered(reply.ID); derr == nil {
							delay = 250 * time.Millisecond
							continue
						} else if report != nil {
							report(derr)
						}
						if deferErr := store.DeferReply(reply.ID); deferErr != nil && report != nil {
							report(deferErr)
						}
						delay = nextDelay(delay, err)
						continue
					} else {
						// Fallback send failed: report original + fallback errors, retry later.
						if report != nil {
							report(err)
							report(ferr)
						}
						if deferErr := store.DeferReply(reply.ID); deferErr != nil && report != nil {
							report(deferErr)
						}
						cancel()
						delay = nextDelay(delay, ferr)
						continue
					}
				}
			} else {
				textIdx := reply.Next - len(reply.Photos)
				if textIdx < 0 || textIdx >= len(reply.Parts) {
					// Corrupt cursor: skip to avoid panic; mark delivered.
					cancel()
					_ = store.Delivered(reply.ID)
					continue
				}
				if reply.Keyboard != nil && textIdx == 0 {
					err = c.SendKeyboard(requestCtx, reply.Route, reply.Parts[textIdx], reply.Keyboard)
				} else {
					err = c.Send(requestCtx, reply.Route, reply.Parts[textIdx])
				}
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
			delay = nextDelay(delay, err)
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

func nextDelay(current time.Duration, err error) time.Duration {
	delay := current * 2
	if delay > 30*time.Second {
		delay = 30 * time.Second
	}
	var api *APIError
	if errors.As(err, &api) && api.RetryAfter > 0 {
		delay = time.Duration(api.RetryAfter) * time.Second
	}
	return delay
}

// isPermanentPhotoError reports photo failures that retrying will not fix:
// missing local files and Telegram 400 rejections (bad file, too large).
// Rate limits and 5xx/network errors remain retryable.
func isPermanentPhotoError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, os.ErrNotExist) {
		return true
	}
	var pathErr *os.PathError
	if errors.As(err, &pathErr) {
		return true
	}
	var api *APIError
	if errors.As(err, &api) {
		if api.Code == 429 || api.Code >= 500 {
			return false
		}
		if api.RetryAfter > 0 {
			return false
		}
		if api.Code == 400 {
			return true
		}
		// Other 4xx: treat as permanent to avoid poison-queue stall.
		if api.Code >= 400 && api.Code < 500 {
			return true
		}
	}
	// Validation errors from SendPhoto (unsupported type, empty, etc.).
	msg := err.Error()
	for _, prefix := range []string{"unsupported photo", "photo exceeds", "photo is empty", "photo is not", "invalid photo", "photo must have"} {
		if len(msg) >= len(prefix) && msg[:len(prefix)] == prefix {
			return true
		}
		// contains check without importing strings (already imported transitively? use manual)
	}
	if contains(msg, "unsupported photo") || contains(msg, "exceeds 10MB") || contains(msg, "not a regular file") || contains(msg, "invalid photo URL") {
		return true
	}
	return false
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }
