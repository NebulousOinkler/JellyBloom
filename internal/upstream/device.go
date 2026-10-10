package upstream

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"sync"
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
	mu    sync.Mutex
	token string
}

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

// DeviceToken returns a token for requests made on behalf of d, logging the
// device in on first use. API-key upstreams use the key, since Jellyfin takes
// an API key request's device from the header.
func (c *Client) DeviceToken(ctx context.Context, d Device) (string, error) {
	if err := c.ensureLogin(ctx, ""); err != nil {
		return "", err
	}
	if c.cfg.APIKey != "" {
		return c.cfg.APIKey, nil
	}
	ds := c.deviceSession(d.ID)
	ds.mu.Lock()
	defer ds.mu.Unlock()
	if ds.token != "" {
		return ds.token, nil
	}
	return ds.token, c.loginDevice(ctx, d, ds)
}

// ReauthDevice discards a device's token if it is still stale and logs in
// again. API-key upstreams re-check the key's user instead.
func (c *Client) ReauthDevice(ctx context.Context, d Device, stale string) error {
	if c.cfg.APIKey != "" {
		return c.ensureLogin(ctx, stale)
	}
	ds := c.deviceSession(d.ID)
	ds.mu.Lock()
	defer ds.mu.Unlock()
	if ds.token != "" && ds.token != stale {
		return nil
	}
	return c.loginDevice(ctx, d, ds)
}

func (c *Client) loginDevice(ctx context.Context, d Device, ds *deviceSession) error {
	body := map[string]string{"Username": c.cfg.Username, "Pw": c.cfg.Password}
	var res struct {
		AccessToken string
		User        struct{ Id string }
	}
	if err := c.sendAuth(ctx, http.MethodPost, "/Users/AuthenticateByName", nil, body, c.DeviceAuthHeader(d, ""), &res); err != nil {
		return fmt.Errorf("upstream %s: device login: %w", c.cfg.Name, err)
	}
	if res.AccessToken == "" {
		return fmt.Errorf("upstream %s: device login: no token", c.cfg.Name)
	}
	ds.token = res.AccessToken
	return nil
}
