// Package pairing connects JellyBloom to an upstream: it obtains a token
// (by default through Quick Connect, so JellyBloom never sees a password),
// checks the account is not an administrator unless allowed, stores the
// token, and revokes any token it replaces.
package pairing

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/NebulousOinkler/JellyBloom/internal/config"
	"github.com/NebulousOinkler/JellyBloom/internal/secrets"
	"github.com/NebulousOinkler/JellyBloom/internal/upstream"
)

// Mode selects how the credential is obtained.
type Mode int

const (
	QuickConnect Mode = iota // approve a code in a Jellyfin app (default)
	Password                 // exchange a password once; it is not kept
	APIKey                   // a server API key (admin-level; needs allow_admin)
)

// Options configures a pairing.
type Options struct {
	Upstream config.Upstream
	Store    secrets.Store
	Mode     Mode
	Out      io.Writer
	// Prompt asks the operator for a value (hidden for secrets).
	Prompt  func(label string, hidden bool) (string, error)
	Poll    time.Duration // Quick Connect polling interval; default 2s
	Timeout time.Duration // how long to wait for approval; default 5m
}

func client(u config.Upstream) (*upstream.Client, error) {
	return upstream.New(u, upstream.Credentials{AllowAdmin: u.AllowAdmin}, upstream.Options{})
}

// Pair obtains, checks, and stores a credential for o.Upstream.
func Pair(ctx context.Context, o Options) error {
	u := o.Upstream
	if len(u.TokenCommand) > 0 {
		return fmt.Errorf("upstream %s gets its credential from token_command; pairing is not used", u.Name)
	}
	if o.Poll == 0 {
		o.Poll = 2 * time.Second
	}
	if o.Timeout == 0 {
		o.Timeout = 5 * time.Minute
	}
	c, err := client(u)
	if err != nil {
		return err
	}

	var sec upstream.Secret
	switch o.Mode {
	case QuickConnect:
		if !c.QuickConnectEnabled(ctx) {
			return fmt.Errorf("Quick Connect is disabled on %s (%s). Ask its administrator to enable it "+
				"(Dashboard → General → Quick Connect), or run `jellybloom pair %s --password`", u.Name, u.URL, u.Name)
		}
		q, err := c.StartPairing(ctx)
		if err != nil {
			return err
		}
		fmt.Fprintf(o.Out, "To pair JellyBloom with %q (%s):\n"+
			"  1. Open Jellyfin on a device signed in to that server as the account JellyBloom should use.\n"+
			"  2. Go to Settings → Quick Connect and enter this code:  %s\n"+
			"Waiting for approval (up to %s)...\n", u.Name, u.URL, q.Code, o.Timeout)
		wctx, cancel := context.WithTimeout(ctx, o.Timeout)
		sec, err = c.FinishPairing(wctx, q, o.Poll)
		cancel()
		if errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("the code was not approved within %s; run pair again for a new code", o.Timeout)
		}
		if err != nil {
			return err
		}
	case Password:
		user := u.Username
		if user == "" {
			if user, err = o.Prompt("Username on "+u.Name, false); err != nil {
				return err
			}
		}
		pw, err := o.Prompt("Password for "+user, true)
		if err != nil {
			return err
		}
		if sec, err = c.PasswordSecret(ctx, user, pw); err != nil {
			return fmt.Errorf("login to %s failed: %w", u.Name, err)
		}
	case APIKey:
		if u.User == "" || !u.AllowAdmin {
			return fmt.Errorf("an API key acts as the user named by `user:` for upstream %s and needs allow_admin: true "+
				"(Jellyfin API keys are admin keys); prefer Quick Connect pairing with a non-admin account", u.Name)
		}
		key, err := o.Prompt("API key for "+u.Name, true)
		if err != nil {
			return err
		}
		sec = upstream.Secret{Kind: upstream.KindAPIKey, Value: key, User: u.User}
	}

	acct, err := c.Account(ctx, sec)
	if err != nil {
		c.Revoke(ctx, sec)
		return fmt.Errorf("checking the account on %s: %w", u.Name, err)
	}
	if acct.Admin && !u.AllowAdmin {
		c.Revoke(ctx, sec)
		return fmt.Errorf("%q is an administrator on %s, and JellyBloom never needs admin rights. Pair a non-administrator "+
			"account (recommended: a dedicated user with access to the libraries you want to share), or set allow_admin: true "+
			"for this upstream", acct.Name, u.Name)
	}

	old, oldErr := secrets.Load(o.Store, u.Name)
	if err := secrets.Save(o.Store, u.Name, sec); err != nil {
		c.Revoke(ctx, sec)
		return fmt.Errorf("storing the credential: %w", err)
	}
	if oldErr == nil && old.Value != sec.Value {
		if err := c.Revoke(ctx, old); err != nil {
			fmt.Fprintf(o.Out, "warning: could not revoke the previous credential: %v\n", err)
		}
	}

	role := "not an administrator"
	if acct.Admin {
		role = "an administrator (allowed by allow_admin)"
	}
	fmt.Fprintf(o.Out, "Paired %q as %q (%s). Credential stored in the %s.\n", u.Name, acct.Name, role, o.Store.Describe())
	if acct.CanDelete {
		fmt.Fprintf(o.Out, "Note: this account may delete media. JellyBloom never does; consider turning off "+
			"\"Allow media deletion\" for it on %s.\n", u.Name)
	}
	if sec.Kind == upstream.KindToken {
		fmt.Fprintf(o.Out, "The server's administrator can revoke this at any time under Dashboard → Devices (\"JellyBloom\").\n")
	}
	return nil
}

// Unpair revokes and deletes the stored credential for u.
func Unpair(ctx context.Context, u config.Upstream, store secrets.Store, out io.Writer) error {
	sec, err := secrets.Load(store, u.Name)
	if errors.Is(err, upstream.ErrNotPaired) {
		return fmt.Errorf("upstream %s is not paired", u.Name)
	}
	if err != nil {
		return err
	}
	c, err := client(u)
	if err != nil {
		return err
	}
	if err := c.Revoke(ctx, sec); err != nil {
		fmt.Fprintf(out, "warning: could not revoke the credential on %s (%v); remove the JellyBloom device there manually\n", u.Name, err)
	}
	if err := secrets.Delete(store, u.Name); err != nil {
		return err
	}
	fmt.Fprintf(out, "Unpaired %q.\n", u.Name)
	return nil
}
