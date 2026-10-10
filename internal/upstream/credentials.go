package upstream

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// Secret kinds.
const (
	KindToken  = "token"   // a user's access token (from pairing or a password exchange)
	KindAPIKey = "api_key" // a server API key (always admin-level in Jellyfin)
)

// Secret is an upstream's long-lived credential. JellyBloom never needs the
// account password itself: tokens can be revoked per device on the upstream
// and do not expose a password that may be reused elsewhere.
type Secret struct {
	Kind  string `json:"kind"`
	Value string `json:"value"`
	User  string `json:"user,omitempty"` // API keys: the user to act as
}

// Credentials supplies an upstream's secret.
type Credentials struct {
	// Load returns the stored secret, or ErrNotPaired.
	Load func(ctx context.Context) (Secret, error)
	// Save persists a token obtained by exchanging the fallback password.
	// Optional.
	Save func(ctx context.Context, s Secret) error
	// Password is an optional fallback for unattended deployments: it is
	// exchanged for a token when no valid secret is stored.
	Password func() (username, password string, err error)
	// AllowAdmin permits an administrator account or API key.
	AllowAdmin bool
}

var (
	// ErrNotPaired: no credential is stored for the upstream.
	ErrNotPaired = errors.New("not paired")
	// ErrRevoked: the upstream rejected the stored credential.
	ErrRevoked = errors.New("stored credential was rejected")
	// ErrAdmin: the credential is an administrator's and allow_admin is off.
	ErrAdmin = errors.New("account is an administrator")
)

// CredentialError is a failure that needs an operator (pairing again, or a
// less privileged account) rather than a retry.
type CredentialError struct {
	Upstream string
	Err      error
	Hint     string
}

func (e *CredentialError) Error() string {
	return fmt.Sprintf("upstream %s: %v; %s", e.Upstream, e.Err, e.Hint)
}

func (e *CredentialError) Unwrap() error { return e.Err }

func (c *Client) credErr(err error) *CredentialError {
	hint := "run `jellybloom pair " + c.cfg.Name + "`"
	if errors.Is(err, ErrAdmin) {
		hint = "pair a non-administrator account (recommended), or set allow_admin: true for this upstream"
	}
	return &CredentialError{Upstream: c.cfg.Name, Err: err, Hint: hint}
}

// Account is the upstream user a credential acts as.
type Account struct {
	ID, Name  string
	Admin     bool
	CanDelete bool
}

func isUnauthorized(err error) bool {
	var se *StatusError
	return errors.As(err, &se) && (se.Code == http.StatusUnauthorized || se.Code == http.StatusForbidden)
}

// Account resolves who s acts as. API keys are admin-level by definition.
func (c *Client) Account(ctx context.Context, s Secret) (*Account, error) {
	if s.Kind == KindAPIKey {
		var users []struct{ Name, Id string }
		if err := c.send(ctx, http.MethodGet, "/Users", nil, nil, s.Value, &users); err != nil {
			return nil, err
		}
		for _, u := range users {
			if strings.EqualFold(u.Name, s.User) {
				return &Account{ID: u.Id, Name: u.Name, Admin: true, CanDelete: true}, nil
			}
		}
		return nil, fmt.Errorf("upstream %s: user %q not found", c.cfg.Name, s.User)
	}
	var me struct {
		Id, Name string
		Policy   struct{ IsAdministrator, EnableContentDeletion bool }
	}
	if err := c.send(ctx, http.MethodGet, "/Users/Me", nil, nil, s.Value, &me); err != nil {
		return nil, err
	}
	return &Account{ID: me.Id, Name: me.Name, Admin: me.Policy.IsAdministrator, CanDelete: me.Policy.EnableContentDeletion}, nil
}

// adopt makes s the client's session after enforcing the admin rule.
func (c *Client) adopt(s Secret, a *Account) error {
	if a.Admin && !c.creds.AllowAdmin {
		return c.credErr(ErrAdmin)
	}
	c.mu.Lock()
	c.token, c.userID, c.apiKey = s.Value, a.ID, s.Kind == KindAPIKey
	c.mu.Unlock()
	return nil
}

// loginLocked establishes a session: the stored secret if the upstream
// still accepts it, else the fallback password. stale is a token the
// upstream just rejected.
func (c *Client) loginLocked(ctx context.Context, stale string) error {
	var s Secret
	err := ErrNotPaired
	if c.creds.Load != nil {
		s, err = c.creds.Load(ctx)
	}
	if err != nil && !errors.Is(err, ErrNotPaired) {
		return fmt.Errorf("upstream %s: load credential: %w", c.cfg.Name, err)
	}
	rejected := false
	if err == nil && s.Value != "" {
		if s.Value == stale {
			rejected = true
		} else {
			a, aerr := c.Account(ctx, s)
			switch {
			case aerr == nil:
				return c.adopt(s, a)
			case !isUnauthorized(aerr):
				return aerr // transient: retry later
			}
			rejected = true
		}
	}

	if c.creds.Password != nil && (s.Kind != KindAPIKey || !rejected) {
		user, pw, perr := c.creds.Password()
		if perr != nil {
			return fmt.Errorf("upstream %s: read password: %w", c.cfg.Name, perr)
		}
		tok, err := c.exchangePassword(ctx, c.authHeader(""), user, pw)
		if err != nil {
			if isUnauthorized(err) {
				return c.credErr(fmt.Errorf("password login rejected: %w", err))
			}
			return fmt.Errorf("upstream %s: login: %w", c.cfg.Name, err)
		}
		s = Secret{Kind: KindToken, Value: tok}
		a, err := c.Account(ctx, s)
		if err != nil {
			return err
		}
		if err := c.adopt(s, a); err != nil {
			c.Revoke(ctx, s) // don't leave an unusable admin session behind
			return err
		}
		if c.creds.Save != nil {
			_ = c.creds.Save(ctx, s) // best effort: the session works either way
		}
		return nil
	}
	if rejected {
		return c.credErr(ErrRevoked)
	}
	return c.credErr(ErrNotPaired)
}

// exchangePassword logs in with a password, returning the token.
func (c *Client) exchangePassword(ctx context.Context, auth, username, password string) (string, error) {
	body := map[string]string{"Username": username, "Pw": password}
	var res struct{ AccessToken string }
	if err := c.sendAuth(ctx, http.MethodPost, "/Users/AuthenticateByName", nil, body, auth, &res); err != nil {
		return "", err
	}
	if res.AccessToken == "" {
		return "", fmt.Errorf("upstream %s: login response has no token", c.cfg.Name)
	}
	return res.AccessToken, nil
}

// Revoke ends the session a token belongs to (it stops working upstream).
// API keys are managed by the server admin and are left alone.
func (c *Client) Revoke(ctx context.Context, s Secret) error {
	if s.Kind == KindAPIKey || s.Value == "" {
		return nil
	}
	err := c.send(ctx, http.MethodPost, "/Sessions/Logout", nil, nil, s.Value, nil)
	if isUnauthorized(err) {
		return nil // already revoked
	}
	return err
}
