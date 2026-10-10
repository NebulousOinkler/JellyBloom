// Package upstream is JellyBloom's client for a single upstream Jellyfin server.
// JellyBloom talks to each upstream exactly as an ordinary Jellyfin client would.
package upstream

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/NebulousOinkler/JellyBloom/internal/config"
)

const (
	clientName = "JellyBloom"
	// Version is reported to upstreams in the MediaBrowser authorization header.
	Version = "0.1.0"
)

// Options tunes a Client. Zero values select defaults.
type Options struct {
	HTTPClient  *http.Client  // default: 30s timeout
	MaxRetries  int           // retries after the first attempt; default 3
	BaseBackoff time.Duration // default 250ms
	MaxBackoff  time.Duration // default 10s
}

// Client is a logged-in session against one upstream. It is safe for concurrent use.
type Client struct {
	cfg      config.Upstream
	base     *url.URL
	deviceID string
	hc       *http.Client

	maxRetries  int
	baseBackoff time.Duration
	maxBackoff  time.Duration

	loginMu sync.Mutex // serializes logins
	mu      sync.RWMutex
	token   string
	userID  string

	devMu   sync.Mutex
	devices map[string]*deviceSession // per-client sessions, by Device.ID
}

// StatusError is a non-2xx response from an upstream.
type StatusError struct {
	Upstream string
	Method   string
	Path     string
	Code     int
	Body     string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("upstream %s: %s %s: HTTP %d: %s", e.Upstream, e.Method, e.Path, e.Code, e.Body)
}

// New creates a client for u. It does not contact the upstream; the first
// authenticated call (or Login) does.
func New(u config.Upstream, opts Options) (*Client, error) {
	base, err := url.Parse(u.URL)
	if err != nil {
		return nil, fmt.Errorf("upstream %q: bad url: %w", u.Name, err)
	}
	if base.Scheme != "http" && base.Scheme != "https" {
		return nil, fmt.Errorf("upstream %q: url must be http or https", u.Name)
	}
	base.Path = strings.TrimRight(base.Path, "/")

	c := &Client{
		cfg:         u,
		base:        base,
		deviceID:    deviceID(u.Name),
		hc:          opts.HTTPClient,
		maxRetries:  opts.MaxRetries,
		baseBackoff: opts.BaseBackoff,
		maxBackoff:  opts.MaxBackoff,
	}
	if c.hc == nil {
		c.hc = &http.Client{Timeout: 30 * time.Second}
	}
	if c.maxRetries == 0 {
		c.maxRetries = 3
	}
	if c.baseBackoff == 0 {
		c.baseBackoff = 250 * time.Millisecond
	}
	if c.maxBackoff == 0 {
		c.maxBackoff = 10 * time.Second
	}
	return c, nil
}

// deviceID is stable per upstream name so the upstream sees one device across restarts.
func deviceID(name string) string {
	sum := sha256.Sum256([]byte("jellybloom:" + name))
	return hex.EncodeToString(sum[:16])
}

// Name returns the configured upstream name.
func (c *Client) Name() string { return c.cfg.Name }

// UserID returns the upstream user ID JellyBloom acts as, or "" before login.
func (c *Client) UserID() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.userID
}

// BaseURL returns a copy of the upstream's base URL (no trailing slash).
func (c *Client) BaseURL() *url.URL {
	u := *c.base
	return &u
}

// AuthHeader returns the MediaBrowser authorization header for token.
func (c *Client) AuthHeader(token string) string { return c.authHeader(token) }

// Session returns the current token and user ID, logging in if needed.
func (c *Client) Session(ctx context.Context) (token, userID string, err error) {
	if err := c.ensureLogin(ctx, ""); err != nil {
		return "", "", err
	}
	token, userID = c.session()
	return token, userID, nil
}

// Reauth discards stale (if it is still current) and logs in again. Use it
// after the upstream rejects a token with 401.
func (c *Client) Reauth(ctx context.Context, stale string) error {
	return c.ensureLogin(ctx, stale)
}

func (c *Client) session() (token, userID string) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.token, c.userID
}

func (c *Client) authHeader(token string) string {
	h := fmt.Sprintf(`MediaBrowser Client="%s", Device="%s", DeviceId="%s", Version="%s"`,
		clientName, clientName, c.deviceID, Version)
	if token != "" {
		h += fmt.Sprintf(`, Token="%s"`, token)
	}
	return h
}

// Login authenticates with the upstream, replacing any current session.
func (c *Client) Login(ctx context.Context) error {
	c.loginMu.Lock()
	defer c.loginMu.Unlock()
	return c.loginLocked(ctx)
}

// ensureLogin logs in unless a session exists. If stale is non-empty and still
// the current token, the session is discarded first (used after a 401).
func (c *Client) ensureLogin(ctx context.Context, stale string) error {
	c.loginMu.Lock()
	defer c.loginMu.Unlock()
	token, _ := c.session()
	if token != "" && token != stale {
		return nil // already logged in, or another goroutine re-logged in
	}
	return c.loginLocked(ctx)
}

func (c *Client) loginLocked(ctx context.Context) error {
	if c.cfg.APIKey != "" {
		var users []struct{ Name, Id string }
		if err := c.send(ctx, http.MethodGet, "/Users", nil, nil, c.cfg.APIKey, &users); err != nil {
			return fmt.Errorf("upstream %s: list users: %w", c.cfg.Name, err)
		}
		for _, u := range users {
			if strings.EqualFold(u.Name, c.cfg.User) {
				c.setSession(c.cfg.APIKey, u.Id)
				return nil
			}
		}
		return fmt.Errorf("upstream %s: user %q not found", c.cfg.Name, c.cfg.User)
	}

	body := map[string]string{"Username": c.cfg.Username, "Pw": c.cfg.Password}
	var res struct {
		AccessToken string
		User        struct{ Id string }
	}
	if err := c.send(ctx, http.MethodPost, "/Users/AuthenticateByName", nil, body, "", &res); err != nil {
		return fmt.Errorf("upstream %s: login: %w", c.cfg.Name, err)
	}
	if res.AccessToken == "" || res.User.Id == "" {
		return fmt.Errorf("upstream %s: login: response missing token or user", c.cfg.Name)
	}
	c.setSession(res.AccessToken, res.User.Id)
	return nil
}

func (c *Client) setSession(token, userID string) {
	c.mu.Lock()
	c.token, c.userID = token, userID
	c.mu.Unlock()
}

// get performs an authenticated GET, decoding JSON into out (if non-nil).
func (c *Client) get(ctx context.Context, path string, query url.Values, out any) error {
	return c.authed(ctx, http.MethodGet, path, query, nil, out)
}

// authed sends an authenticated request, logging in first if needed and
// re-logging in once if the upstream rejects the token.
func (c *Client) authed(ctx context.Context, method, path string, query url.Values, body, out any) error {
	if err := c.ensureLogin(ctx, ""); err != nil {
		return err
	}
	token, _ := c.session()
	err := c.send(ctx, method, path, query, body, token, out)
	var se *StatusError
	if errors.As(err, &se) && se.Code == http.StatusUnauthorized {
		if err := c.ensureLogin(ctx, token); err != nil {
			return err
		}
		token, _ = c.session()
		err = c.send(ctx, method, path, query, body, token, out)
	}
	return err
}

// send performs one logical request with retries for transient failures.
func (c *Client) send(ctx context.Context, method, path string, query url.Values, body any, token string, out any) error {
	return c.sendAuth(ctx, method, path, query, body, c.authHeader(token), out)
}

// sendAuth is send with an explicit Authorization header.
func (c *Client) sendAuth(ctx context.Context, method, path string, query url.Values, body any, auth string, out any) error {
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return err
		}
	}
	idempotent := method == http.MethodGet || method == http.MethodHead

	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, method, c.url(path, query), bytes.NewReader(payload))
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", auth)
		req.Header.Set("Accept", "application/json")
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}

		resp, err := c.hc.Do(req)
		var retryAfter time.Duration
		if err == nil {
			err = c.handle(resp, method, path, out)
			retryAfter = parseRetryAfter(resp.Header.Get("Retry-After"))
		}
		if err == nil || !c.retryable(err, idempotent) || attempt >= c.maxRetries || ctx.Err() != nil {
			return err
		}
		if err := sleep(ctx, min(max(retryAfter, c.backoff(attempt)), c.maxBackoff)); err != nil {
			return err
		}
	}
}

func (c *Client) handle(resp *http.Response, method, path string, out any) error {
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return &StatusError{Upstream: c.cfg.Name, Method: method, Path: path, Code: resp.StatusCode, Body: strings.TrimSpace(string(msg))}
	}
	if out == nil || resp.StatusCode == http.StatusNoContent {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("upstream %s: %s %s: decode: %w", c.cfg.Name, method, path, err)
	}
	return nil
}

// retryable reports whether err is transient. Non-idempotent requests are only
// retried when the upstream signals it did not process them (429, 503).
func (c *Client) retryable(err error, idempotent bool) bool {
	var se *StatusError
	if errors.As(err, &se) {
		switch se.Code {
		case http.StatusTooManyRequests, http.StatusServiceUnavailable:
			return true
		case http.StatusBadGateway, http.StatusGatewayTimeout, http.StatusInternalServerError:
			return idempotent
		}
		return false
	}
	return idempotent // network error
}

// backoff returns a full-jitter exponential delay for the given attempt.
func (c *Client) backoff(attempt int) time.Duration {
	d := c.baseBackoff << min(attempt, 20)
	if d <= 0 || d > c.maxBackoff {
		d = c.maxBackoff
	}
	return time.Duration(rand.Int64N(int64(d)) + 1)
}

func parseRetryAfter(v string) time.Duration {
	if v == "" {
		return 0
	}
	if s, err := strconv.Atoi(v); err == nil && s > 0 {
		return time.Duration(s) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		return time.Until(t)
	}
	return 0
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (c *Client) url(path string, query url.Values) string {
	u := *c.base
	u.Path = c.base.Path + path
	u.RawQuery = query.Encode()
	return u.String()
}
