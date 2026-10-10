// Package config loads JellyBloom's YAML configuration.
package config

import (
	"fmt"
	"os"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

type Upstream struct {
	Name     string `yaml:"name"`
	URL      string `yaml:"url"`
	Username string `yaml:"username"`
	Password string `yaml:"password"`
	APIKey   string `yaml:"api_key"`
	User     string `yaml:"user"` // user to act as when using APIKey
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
	Upstreams   []Upstream `yaml:"upstreams"`
	Users       []User     `yaml:"users"`
	// FallbackWriteAllow lists extra regexes (matched against the lowercased
	// path) for non-GET requests the generic fallback may forward, e.g. for
	// plugin endpoints. Built-in rules cover playback and user data.
	FallbackWriteAllow []string `yaml:"fallback_write_allow"`
}

func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	c := &Config{Listen: ":8096", ServerName: "JellyBloom", Merge: "union", DataDir: "data"}
	if err := yaml.Unmarshal(b, c); err != nil {
		return nil, err
	}
	switch c.Merge {
	case "union", "intersection", "difference":
	default:
		return nil, fmt.Errorf("unknown merge mode %q", c.Merge)
	}
	if len(c.Upstreams) == 0 {
		return nil, fmt.Errorf("no upstreams configured")
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
		if u.APIKey == "" && u.Username == "" {
			return nil, fmt.Errorf("upstream %q: need api_key or username/password", u.Name)
		}
		if u.APIKey != "" && u.User == "" {
			return nil, fmt.Errorf("upstream %q: api_key requires user", u.Name)
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
