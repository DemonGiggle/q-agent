package console

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"q-agent/internal/chatgptoauth"
)

// login handles `q-agent login chatgpt`. Console-only, terminal-required,
// manual paste-back: no localhost listener, no Telegram.
func (c *console) login(args []string) error {
	if c.json {
		return fmt.Errorf("login is interactive and does not support --json")
	}
	if !c.terminal {
		return fmt.Errorf("login requires an interactive terminal")
	}
	if len(args) != 1 || args[0] != "chatgpt" {
		return fmt.Errorf("usage: q-agent login chatgpt [--config FILE]")
	}
	cfg, err := c.load()
	if err != nil {
		return err
	}
	oauthPath := cfg.ChatGPT.OAuthPath
	if oauthPath == "" {
		oauthPath = chatgptoauth.DefaultPath
	}
	verifier, err := chatgptoauth.NewVerifier()
	if err != nil {
		return err
	}
	state, err := chatgptoauth.NewState()
	if err != nil {
		return err
	}
	fmt.Fprintln(c.out, "ChatGPT login (console only).")
	fmt.Fprintln(c.out, "")
	fmt.Fprintln(c.out, "1. Open this URL in your browser and approve:")
	fmt.Fprintln(c.out, "")
	fmt.Fprintln(c.out, "   "+chatgptoauth.AuthorizeURL(verifier, state))
	fmt.Fprintln(c.out, "")
	fmt.Fprintln(c.out, "2. Your browser will try to reach http://localhost:1455/auth/callback")
	fmt.Fprintln(c.out, "   and fail to connect. That is expected.")
	fmt.Fprintln(c.out, "3. Paste the full URL from the address bar below.")
	fmt.Fprintln(c.out, "")
	pasted, err := c.prompt("Callback URL> ")
	if err != nil {
		return err
	}
	code, err := chatgptoauth.ParseCallback(pasted, state)
	if err != nil {
		return err
	}
	httpClient := &http.Client{Timeout: 30 * time.Second}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tokens, err := chatgptoauth.Exchange(ctx, httpClient, code, verifier)
	if err != nil {
		return fmt.Errorf("login failed: %w", err)
	}
	if err := chatgptoauth.Save(oauthPath, tokens); err != nil {
		return err
	}
	resolved, _ := chatgptoauth.ResolvePath(oauthPath)
	fmt.Fprintln(c.out, "")
	fmt.Fprintf(c.out, "Saved %s (0600).\n", resolved)
	fmt.Fprintln(c.out, "Next: set provider.name = \"chatgpt\" and provider.model to a Codex model,")
	fmt.Fprintln(c.out, "then restart the service. Example:")
	fmt.Fprintln(c.out, "  q-agent models set-default --provider chatgpt --model gpt-5.6-codex")
	return nil
}
