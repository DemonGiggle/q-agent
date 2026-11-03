package console

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"q-agent/internal/chatgptoauth"
)

// login handles `q-agent login chatgpt` and `q-agent logout chatgpt`.
// Console-only, terminal-required. New accounts register with
// client_id=dynamic_agent_client; returning accounts reuse the saved issued
// client_id with id_token/login hints. A 127.0.0.1 loopback listener captures
// the callback; manual paste-back remains as a fallback for remote hosts.
func (c *console) login(args []string) error {
	if c.json {
		return fmt.Errorf("login is interactive and does not support --json")
	}
	if !c.terminal {
		return fmt.Errorf("login requires an interactive terminal")
	}
	if len(args) == 0 {
		return fmt.Errorf("usage: q-agent login chatgpt [--config FILE] | q-agent logout chatgpt [--config FILE]")
	}
	if len(args) == 1 && args[0] == "chatgpt" {
		return c.loginChatGPT()
	}
	// Accept `q-agent login chatgpt` with flags already stripped by options(),
	// and `q-agent logout chatgpt` routed here for symmetry (see run()).
	return fmt.Errorf("usage: q-agent login chatgpt [--config FILE]")
}

func (c *console) logout(args []string) error {
	if c.json {
		return fmt.Errorf("logout is interactive and does not support --json")
	}
	if !c.terminal {
		return fmt.Errorf("logout requires an interactive terminal")
	}
	if len(args) != 1 || args[0] != "chatgpt" {
		return fmt.Errorf("usage: q-agent logout chatgpt [--config FILE]")
	}
	cfg, err := c.load()
	if err != nil {
		return err
	}
	oauthPath := cfg.ChatGPT.OAuthPath
	if oauthPath == "" {
		oauthPath = chatgptoauth.DefaultPath
	}
	stored, err := chatgptoauth.Load(oauthPath)
	if err != nil {
		return fmt.Errorf("nothing to sign out: %w", err)
	}
	httpClient := &http.Client{Timeout: 30 * time.Second}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if stored.RefreshToken != "" && stored.ClientID != "" {
		if err := chatgptoauth.Revoke(ctx, httpClient, stored.RefreshToken, stored.ClientID); err != nil {
			fmt.Fprintf(c.out, "Warning: remote revocation not confirmed: %v\n", err)
			fmt.Fprintln(c.out, "Local tokens cleared. Disconnect the app in ChatGPT Settings if needed.")
		} else {
			fmt.Fprintln(c.out, "Revoked the renewable session.")
		}
	}
	if err := chatgptoauth.ClearTokens(oauthPath); err != nil {
		return err
	}
	fmt.Fprintln(c.out, "Signed out. The host ID and account mapping are retained for the next sign-in.")
	fmt.Fprintln(c.out, "To disconnect fully, remove the app in ChatGPT Settings as well.")
	return nil
}

func (c *console) loginChatGPT() error {
	cfg, err := c.load()
	if err != nil {
		return err
	}
	oauthPath := cfg.ChatGPT.OAuthPath
	if oauthPath == "" {
		oauthPath = chatgptoauth.DefaultPath
	}
	hostPath := cfg.ChatGPT.HostPath
	if hostPath == "" {
		hostPath = chatgptoauth.DefaultHostPath
	}

	// Returning account? Reuse its issued client_id and hints. Keep the new
	// attempt separate until its identity is validated.
	var saved chatgptoauth.Tokens
	hasSaved := false
	if t, err := chatgptoauth.Load(oauthPath); err == nil && !t.IsLegacy() && t.ClientID != "" {
		saved = t
		hasSaved = true
	} else if err == nil && t.IsLegacy() {
		fmt.Fprintln(c.out, "Existing login predates Sign in with ChatGPT and must be replaced.")
	}

	hostID, err := chatgptoauth.EnsureHostID(hostPath)
	if err != nil {
		return err
	}
	verifier, err := chatgptoauth.NewVerifier()
	if err != nil {
		return err
	}
	state, err := chatgptoauth.NewState()
	if err != nil {
		return err
	}
	nonce, err := chatgptoauth.NewNonce()
	if err != nil {
		return err
	}

	isNew := !hasSaved
	pendingClient := chatgptoauth.DynamicClientID
	idHint, loginHint := "", ""
	if hasSaved {
		pendingClient = saved.ClientID
		isNew = false
		idHint = saved.IDToken
		loginHint = saved.Email
	}

	ln, redirectURI, err := chatgptoauth.StartLoopbackListener(chatgptoauth.PreferredCallbackPort)
	if err != nil {
		return err
	}
	defer ln.Close()

	authURL := chatgptoauth.AuthorizeURL(chatgptoauth.AuthorizeParams{
		ClientID:     pendingClient,
		RedirectURI:  redirectURI,
		State:        state,
		Nonce:        nonce,
		Verifier:     verifier,
		HostID:       hostID,
		IDTokenHint:  idHint,
		LoginHint:    loginHint,
		IsNewAccount: isNew,
	})

	fmt.Fprintln(c.out, "ChatGPT login (console only).")
	fmt.Fprintln(c.out, "")
	fmt.Fprintln(c.out, "Continue with ChatGPT in your browser to approve:")
	fmt.Fprintln(c.out, "")
	fmt.Fprintln(c.out, "   "+authURL)
	fmt.Fprintln(c.out, "")
	fmt.Fprintf(c.out, "Waiting on %s for approval (up to 5 minutes).\n", redirectURI)
	fmt.Fprintln(c.out, "If the browser is on this host, approve there and return here.")
	fmt.Fprintln(c.out, "If the browser is on another host, paste the full address-bar URL below.")
	fmt.Fprintln(c.out, "")

	pasted, err := c.prompt("Callback URL (press Enter to wait for this host)> ")
	if err != nil {
		return err
	}
	var cb chatgptoauth.CallbackResult
	if strings.TrimSpace(pasted) != "" {
		parsed, err := chatgptoauth.ParseCallback(pasted, state, pendingClient, isNew)
		if err != nil {
			return err
		}
		cb = parsed
		ln.Close()
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		got, _, err := chatgptoauth.WaitForCallback(ctx, ln, state, pendingClient, isNew)
		if err != nil {
			return fmt.Errorf("login failed: %w", err)
		}
		cb = got
	}

	httpClient := &http.Client{Timeout: 30 * time.Second}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	tokens, err := c.exchangeValidated(ctx, httpClient, cb, verifier, redirectURI, hostID, nonce, pendingClient, isNew)
	if err != nil {
		var planErr *chatgptoauth.ErrPlanNotEnabled
		if errors.As(err, &planErr) {
			// Retain the sign-in but flag plan usage as disabled per the
			// errors-and-recovery guide.
			if saveErr := chatgptoauth.Save(oauthPath, planErr.Tokens()); saveErr != nil {
				return saveErr
			}
			fmt.Fprintln(c.out, "")
			fmt.Fprintln(c.out, "Signed in, but ChatGPT plan use isn't enabled for this authorization.")
			fmt.Fprintln(c.out, "Re-run login and approve plan usage, or configure another billing path (API key).")
			fmt.Fprintln(c.out, "Manage access in ChatGPT Settings → Usage.")
			return nil
		}
		var grantErr *chatgptoauth.ErrInvalidGrant
		if errors.As(err, &grantErr) && isNew && cb.ClientID != "" {
			// Discard the spent code and retry once with the retained issued ID.
			return fmt.Errorf("login failed (code expired); run the command again to re-authorize %s", cb.ClientID)
		}
		return fmt.Errorf("login failed: %w", err)
	}

	// For a returning account, confirm the new identity matches the selected
	// account before replacing credentials.
	if hasSaved && tokens.Subject != "" && saved.Subject != "" && tokens.Subject != saved.Subject {
		return fmt.Errorf("sign-in identity changed accounts; run `q-agent logout chatgpt` first or register it as a new account")
	}

	if err := chatgptoauth.Save(oauthPath, tokens); err != nil {
		return err
	}
	resolved, _ := chatgptoauth.ResolvePath(oauthPath)
	fmt.Fprintln(c.out, "")
	fmt.Fprintf(c.out, "Saved %s (0600).\n", resolved)
	if !tokens.HasPlanScope() {
		fmt.Fprintln(c.out, "Warning: granted scopes lack chatgpt.tokens.use.direct; inference will be refused.")
	}
	fmt.Fprintln(c.out, "Next: set provider.name = \"chatgpt\" and provider.model to a Responses model,")
	fmt.Fprintln(c.out, "then restart the service. Example:")
	fmt.Fprintln(c.out, "  q-agent models set-default --provider chatgpt --model gpt-6.1-sol")
	return nil
}

func (c *console) exchangeValidated(ctx context.Context, httpClient *http.Client, cb chatgptoauth.CallbackResult, verifier, redirectURI, hostID, nonce, pendingClient string, isNew bool) (chatgptoauth.Tokens, error) {
	issued := cb.ClientID
	if issued == "" {
		issued = pendingClient
	}
	if isNew && (issued == "" || issued == chatgptoauth.DynamicClientID) {
		return chatgptoauth.Tokens{}, fmt.Errorf("registration incomplete: callback did not include an issued client_id")
	}
	jwks, err := chatgptoauth.FetchJWKS(ctx, httpClient)
	if err != nil {
		return chatgptoauth.Tokens{}, err
	}
	tokens, err := chatgptoauth.ExchangeValidated(ctx, httpClient, jwks, cb.Code, verifier, issued, redirectURI, hostID, nonce, time.Now())
	if err != nil {
		return chatgptoauth.Tokens{}, err
	}
	return tokens, nil
}
