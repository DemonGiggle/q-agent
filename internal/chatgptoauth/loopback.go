package chatgptoauth

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"
)

// StartLoopbackListener binds 127.0.0.1:preferredPort (/auth/callback) or an
// ephemeral port on conflict. Only the port may vary; scheme, host, and path
// stay fixed.
func StartLoopbackListener(preferredPort int) (net.Listener, string, error) {
	addr := fmt.Sprintf("127.0.0.1:%d", preferredPort)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		ln, err = net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, "", fmt.Errorf("listen loopback: %w", err)
		}
	}
	port := ln.Addr().(*net.TCPAddr).Port
	return ln, CallbackURL(port), nil
}

// WaitForCallback serves a single GET to /auth/callback, validates state,
// and returns the parsed result. It writes a small success/failure page so
// the user can return to the terminal. pendingClientID/isNewAccount follow
// ParseCallback semantics.
func WaitForCallback(ctx context.Context, ln net.Listener, wantState, pendingClientID string, isNewAccount bool) (CallbackResult, string, error) {
	resultCh := make(chan callbackOutcome, 1)
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != CallbackPath {
				http.NotFound(w, r)
				return
			}
			res, err := ParseCallback(r.URL.RequestURI(), wantState, pendingClientID, isNewAccount)
			if err != nil {
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprintln(w, "Sign-in failed. Return to the terminal for details.")
				select {
				case resultCh <- callbackOutcome{err: err}:
				default:
				}
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprintln(w, "<html><body><h1>Signed in with ChatGPT</h1><p>Return to the terminal to finish setup.</p></body></html>")
			select {
			case resultCh <- callbackOutcome{result: res, raw: r.URL.RequestURI()}:
			default:
			}
		}),
	}
	go func() {
		_ = srv.Serve(ln)
	}()
	defer srv.Close()

	select {
	case <-ctx.Done():
		return CallbackResult{}, "", ctx.Err()
	case out := <-resultCh:
		return out.result, out.raw, out.err
	}
}

type callbackOutcome struct {
	result CallbackResult
	raw    string
	err    error
}

// AwaitLoopback is a convenience wrapper with a timeout for login flows.
func AwaitLoopback(ctx context.Context, ln net.Listener, wantState, pendingClientID string, isNewAccount bool, timeout time.Duration) (CallbackResult, string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return WaitForCallback(ctx, ln, wantState, pendingClientID, isNewAccount)
}

var _ = strings.TrimSpace
