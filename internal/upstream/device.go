package upstream

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Device is a client device JellyBloom acts for on an upstream. Giving each
// client its own upstream device keeps upstream sessions and transcodes of
// different clients apart (Jellyfin tracks both per device).
type Device struct {
	ID      string // stable per JellyBloom user and client device; see DeviceID
	Name    string // client device name, e.g. "Firefox"
	Client  string // client app, e.g. "Jellyfin Web"
	Version string // client app version
}

// DeviceID derives a stable upstream device ID for a JellyBloom user's client
// device on one upstream.
func DeviceID(upstream, userID, clientDeviceID string) string {
	sum := sha256.Sum256([]byte("jellybloom-device:" + upstream + ":" + userID + ":" + clientDeviceID))
	return hex.EncodeToString(sum[:16])
}

type deviceSession struct {
	mu      sync.Mutex
	token   string
	retryAt time.Time // minting failed: share JellyBloom's session until then
}

var (
	// mintTimeout bounds giving a device its own session, so a hung
	// upstream cannot hold up a client's requests for long.
	mintTimeout = 10 * time.Second
	// mintRetry is how long a device shares JellyBloom's session after
	// minting failed, before trying again.
	mintRetry = time.Minute
)

func quoteParam(v string) string { return strings.ReplaceAll(v, `"`, "") }

// DeviceAuthHeader returns the MediaBrowser header identifying d.
func (c *Client) DeviceAuthHeader(d Device, token string) string {
	client, name, version := d.Client, d.Name, d.Version
	if client == "" {
		client = clientName
	}
	if name == "" {
		name = clientName
	}
	if version == "" {
		version = Version
	}
	h := fmt.Sprintf(`MediaBrowser Client="%s", Device="%s", DeviceId="%s", Version="%s"`,
		quoteParam(client), quoteParam(name), d.ID, quoteParam(version))
	if token != "" {
		h += fmt.Sprintf(`, Token="%s"`, token)
	}
	return h
}

func (c *Client) deviceSession(id string) *deviceSession {
	c.devMu.Lock()
	defer c.devMu.Unlock()
	if c.devices == nil {
		c.devices = map[string]*deviceSession{}
	}
	ds := c.devices[id]
	if ds == nil {
		ds = &deviceSession{}
		c.devices[id] = ds
	}
	return ds
}

// errNoDeviceLogin: the upstream offers no way to give d its own session.
var errNoDeviceLogin = errors.New("no per-device login available")

// DeviceToken returns a token for requests made on behalf of d. perDevice
// reports whether it is d's own session; when false (no Quick Connect and no
// fallback password), it is JellyBloom's shared session and requests should
// use AuthHeader rather than DeviceAuthHeader. API keys serve every device:
// Jellyfin takes an API key request's device from the header.
func (c *Client) DeviceToken(ctx context.Context, d Device) (token string, perDevice bool, err error) {
	if err := c.ensureLogin(ctx, ""); err != nil {
		return "", false, err
	}
	root, uid := c.session()
	if c.isAPIKey() {
		return root, true, nil
	}
	ds := c.deviceSession(d.ID)
	ds.mu.Lock()
	defer ds.mu.Unlock()
	if ds.token != "" {
		return ds.token, true, nil
	}
	if time.Now().Before(ds.retryAt) {
		return root, false, nil
	}
	mctx, cancel := context.WithTimeout(ctx, mintTimeout)
	defer cancel()
	tok, err := c.mintDevice(mctx, d, root, uid)
	if isUnauthorized(err) {
		// Our own session was rejected (e.g. revoked upstream): re-establish
		// it, which reports a CredentialError if re-pairing is needed.
		if err := c.ensureLogin(ctx, root); err != nil {
			return "", false, err
		}
		root, uid = c.session()
		tok, err = c.mintDevice(mctx, d, root, uid)
	}
	var credErr *CredentialError
	switch {
	case errors.Is(err, errNoDeviceLogin):
		return root, false, nil
	case err != nil && ctx.Err() == nil && !errors.As(err, &credErr):
		// The upstream is slow or failing: serve this request on our own
		// session rather than fail it, and try again later.
		ds.retryAt = time.Now().Add(mintRetry)
		return root, false, nil
	case err != nil:
		return "", false, err
	}
	ds.token = tok
	return tok, true, nil
}

// ReauthDevice replaces d's token if it is still stale (the upstream just
// rejected it). If minting fails because JellyBloom's own session was
// revoked, that session is re-established first.
func (c *Client) ReauthDevice(ctx context.Context, d Device, stale string) error {
	if c.isAPIKey() {
		return c.ensureLogin(ctx, stale)
	}
	ds := c.deviceSession(d.ID)
	ds.mu.Lock()
	defer ds.mu.Unlock()
	if ds.token != "" && ds.token != stale {
		return nil
	}
	ds.token = ""
	root, uid := c.session()
	if stale == root { // a shared-session request was rejected
		return c.ensureLogin(ctx, stale)
	}
	tok, err := c.mintDevice(ctx, d, root, uid)
	if isUnauthorized(err) {
		if err := c.ensureLogin(ctx, root); err != nil {
			return err
		}
		root, uid = c.session()
		tok, err = c.mintDevice(ctx, d, root, uid)
	}
	if err != nil && !errors.Is(err, errNoDeviceLogin) {
		return err
	}
	ds.token = tok
	return nil
}

// mintDevice gives d its own upstream session: through Quick Connect,
// approved with JellyBloom's own session, or with the fallback password.
func (c *Client) mintDevice(ctx context.Context, d Device, root, uid string) (string, error) {
	if c.QuickConnectEnabled(ctx) {
		q, err := c.startQuickConnect(ctx, c.DeviceAuthHeader(d, ""))
		if err != nil {
			return "", err
		}
		query := url.Values{"code": {q.Code}, "userId": {uid}}
		if err := c.send(ctx, http.MethodPost, "/QuickConnect/Authorize", query, nil, root, nil); err != nil {
			return "", err
		}
		return c.finishQuickConnect(ctx, q, c.DeviceAuthHeader(d, ""))
	}
	if c.creds.Password != nil {
		user, pw, err := c.creds.Password()
		if err != nil {
			return "", err
		}
		return c.exchangePassword(ctx, c.DeviceAuthHeader(d, ""), user, pw)
	}
	c.sharedDevices.Store(true)
	return "", errNoDeviceLogin
}

// SharedDevices reports whether clients have had to share JellyBloom's own
// upstream session (Quick Connect off and no fallback password).
func (c *Client) SharedDevices() bool { return c.sharedDevices.Load() }
