package upstream

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// QuickConnectCode is a pending Quick Connect request: someone signed in to
// the upstream approves Code, and the requester then exchanges Secret for a
// token. This is how JellyBloom pairs without ever seeing a password.
type QuickConnectCode struct {
	Code   string
	Secret string
}

// QuickConnectEnabled reports (and caches for 10 minutes) whether the
// upstream allows Quick Connect.
func (c *Client) QuickConnectEnabled(ctx context.Context) bool {
	c.qcMu.Lock()
	defer c.qcMu.Unlock()
	if !c.qcChecked.IsZero() && time.Since(c.qcChecked) < 10*time.Minute {
		return c.qcEnabled
	}
	var on bool
	err := c.send(ctx, http.MethodGet, "/QuickConnect/Enabled", nil, nil, "", &on)
	c.qcEnabled, c.qcChecked = err == nil && on, time.Now()
	return c.qcEnabled
}

func (c *Client) startQuickConnect(ctx context.Context, auth string) (*QuickConnectCode, error) {
	var q QuickConnectCode
	if err := c.sendAuth(ctx, http.MethodPost, "/QuickConnect/Initiate", nil, nil, auth, &q); err != nil {
		return nil, err
	}
	if q.Code == "" || q.Secret == "" {
		return nil, fmt.Errorf("upstream %s: Quick Connect returned no code", c.cfg.Name)
	}
	return &q, nil
}

func (c *Client) finishQuickConnect(ctx context.Context, q *QuickConnectCode, auth string) (string, error) {
	var res struct{ AccessToken string }
	if err := c.sendAuth(ctx, http.MethodPost, "/Users/AuthenticateWithQuickConnect", nil,
		map[string]string{"Secret": q.Secret}, auth, &res); err != nil {
		return "", err
	}
	if res.AccessToken == "" {
		return "", fmt.Errorf("upstream %s: Quick Connect returned no token", c.cfg.Name)
	}
	return res.AccessToken, nil
}

// StartPairing begins Quick Connect pairing as JellyBloom's own device.
func (c *Client) StartPairing(ctx context.Context) (*QuickConnectCode, error) {
	return c.startQuickConnect(ctx, c.authHeader(""))
}

// FinishPairing waits (polling every poll) until q is approved upstream, then
// returns the new token. It stops when ctx ends.
func (c *Client) FinishPairing(ctx context.Context, q *QuickConnectCode, poll time.Duration) (Secret, error) {
	for {
		var st struct{ Authenticated bool }
		err := c.send(ctx, http.MethodGet, "/QuickConnect/Connect", url.Values{"secret": {q.Secret}}, nil, "", &st)
		if err != nil {
			return Secret{}, err
		}
		if st.Authenticated {
			tok, err := c.finishQuickConnect(ctx, q, c.authHeader(""))
			return Secret{Kind: KindToken, Value: tok}, err
		}
		if err := sleep(ctx, poll); err != nil {
			return Secret{}, err
		}
	}
}

// PasswordSecret exchanges a password for a token as JellyBloom's own
// device. The password is not kept.
func (c *Client) PasswordSecret(ctx context.Context, username, password string) (Secret, error) {
	tok, err := c.exchangePassword(ctx, c.authHeader(""), username, password)
	return Secret{Kind: KindToken, Value: tok}, err
}
