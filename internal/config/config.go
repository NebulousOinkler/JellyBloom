// Package config loads JellyBloom's YAML configuration.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Upstream is one Jellyfin server. It holds no secrets: credentials come
// from pairing (`jellybloom pair NAME`, stored in the secret store), a
// token_command, or a password_file.
type Upstream struct {
	Name string `yaml:"name"`
	URL  string `yaml:"url"`
	// AllowAdmin permits an administrator account (or an API key, which is
	// always admin-level). Off by default: JellyBloom blocks admin routes, so
	// an admin credential only adds risk.
	AllowAdmin bool `yaml:"allow_admin"`
	// TokenCommand prints the credential on stdout (argv, no shell), e.g. a
	// password manager CLI. It yields a user token, or an API key when User
	// is set.
	TokenCommand []string `yaml:"token_command"`
	// User is the account an API key acts as (API-key mode only).
	User string `yaml:"user"`
	// PasswordFile and Username are for unattended deployments: the password
	// is exchanged for a token, which is then kept in the secret store.
	Username     string `yaml:"username"`
	PasswordFile string `yaml:"password_file"`
}

// User is a JellyBloom account. Clients log in to JellyBloom, never to upstreams.
type User struct {
	Name         string `yaml:"name"`
	PasswordHash string `yaml:"password_hash"` // bcrypt; generate with `jellybloom hash-password`
	Admin        bool   `yaml:"admin"`
}

type Config struct {
	Listen      string     `yaml:"listen"`
	ServerName  string     `yaml:"server_name"`
	Merge       string     `yaml:"merge"`
	Base        string     `yaml:"base"`     // difference: A in A \ B
	Subtract    []string   `yaml:"subtract"` // difference: B (one or more upstreams)
	WebUpstream string     `yaml:"web_upstream"`
	DataDir     string     `yaml:"data_dir"`
	SecretStore string     `yaml:"secret_store"` // "file" (default, under data_dir/secrets) or "keychain"
	Upstreams   []Upstream `yaml:"upstreams"`
	Users       []User     `yaml:"users"`
	// FallbackWriteAllow lists extra regexes (matched against the lowercased
	// path) for non-GET requests the generic fallback may forward, e.g. for
	// plugin endpoints. Built-in rules cover playback and user data.
	FallbackWriteAllow []string `yaml:"fallback_write_allow"`
	// StallTimeout is how long a relayed file may stop sending before
	// JellyBloom fails over to another copy (default 10s).
	StallTimeout time.Duration `yaml:"stall_timeout"`
}

var upstreamName = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	c := &Config{Listen: ":8096", ServerName: "JellyBloom", Merge: "union", DataDir: "data", SecretStore: "file"}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true) // typos and removed fields are errors, not silently ignored
	if err := dec.Decode(c); err != nil && !errors.Is(err, io.EOF) {
		msg := err.Error()
		for _, removed := range []string{"field password ", "field api_key "} {
			if strings.Contains(msg, removed) {
				return nil, fmt.Errorf("%v: upstream secrets no longer go in the config; pair with `jellybloom pair NAME` (or use password_file / token_command)", err)
			}
		}
		return nil, err
	}
	switch c.SecretStore {
	case "file", "keychain":
	default:
		return nil, fmt.Errorf("unknown secret_store %q (want file or keychain)", c.SecretStore)
	}
	switch c.Merge {
	case "union", "intersection", "difference":
	default:
		return nil, fmt.Errorf("unknown merge mode %q", c.Merge)
	}
	if len(c.Upstreams) == 0 {
		return nil, fmt.Errorf("no upstreams configured")
	}
	if c.StallTimeout < 0 {
		return nil, fmt.Errorf("stall_timeout must not be negative")
	}
	names := map[string]bool{}
	for i, u := range c.Upstreams {
		if u.Name == "" || u.URL == "" {
			return nil, fmt.Errorf("upstream %d: name and url are required", i)
		}
		if names[u.Name] {
			return nil, fmt.Errorf("duplicate upstream name %q", u.Name)
		}
		names[u.Name] = true
		if !upstreamName.MatchString(u.Name) {
			return nil, fmt.Errorf("upstream %q: name may only contain letters, digits, '-' and '_'", u.Name)
		}
		if (u.PasswordFile == "") != (u.Username == "") {
			return nil, fmt.Errorf("upstream %q: username and password_file go together", u.Name)
		}
		if u.PasswordFile != "" && len(u.TokenCommand) > 0 {
			return nil, fmt.Errorf("upstream %q: use either token_command or password_file, not both", u.Name)
		}
		if u.User != "" && !u.AllowAdmin {
			return nil, fmt.Errorf("upstream %q: user (API-key mode) requires allow_admin: true, since Jellyfin API keys are admin keys", u.Name)
		}
	}
	if c.Merge == "difference" {
		if !names[c.Base] {
			return nil, fmt.Errorf("difference: base %q is not a configured upstream", c.Base)
		}
		if len(c.Subtract) == 0 {
			return nil, fmt.Errorf("difference: subtract must list at least one upstream")
		}
		for _, n := range c.Subtract {
			if !names[n] {
				return nil, fmt.Errorf("difference: subtract %q is not a configured upstream", n)
			}
			if n == c.Base {
				return nil, fmt.Errorf("difference: %q cannot be both base and subtract", n)
			}
		}
	} else if c.Base != "" || len(c.Subtract) > 0 {
		return nil, fmt.Errorf("base and subtract only apply to merge: difference")
	}
	if len(c.Users) == 0 {
		return nil, fmt.Errorf("at least one user is required")
	}
	users := map[string]bool{}
	for i, u := range c.Users {
		if u.Name == "" {
			return nil, fmt.Errorf("user %d: name is required", i)
		}
		if users[strings.ToLower(u.Name)] {
			return nil, fmt.Errorf("duplicate user %q", u.Name)
		}
		users[strings.ToLower(u.Name)] = true
		if !strings.HasPrefix(u.PasswordHash, "$2") {
			return nil, fmt.Errorf("user %q: password_hash must be a bcrypt hash (see `jellybloom hash-password`)", u.Name)
		}
	}
	for _, re := range c.FallbackWriteAllow {
		if _, err := regexp.Compile(re); err != nil {
			return nil, fmt.Errorf("fallback_write_allow: %w", err)
		}
	}
	if c.WebUpstream == "" {
		c.WebUpstream = c.Upstreams[0].Name
	} else if !names[c.WebUpstream] {
		return nil, fmt.Errorf("web_upstream %q is not a configured upstream", c.WebUpstream)
	}
	return c, nil
}
