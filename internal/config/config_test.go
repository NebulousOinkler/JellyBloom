package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func load(t *testing.T, yaml string) (*Config, error) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	return Load(p)
}

func TestLoadDefaults(t *testing.T) {
	c, err := load(t, `
upstreams:
  - {name: home, url: "http://h:8096"}
  - {name: friend, url: "https://f", user: guest, allow_admin: true, token_command: [op, read, "op://x"]}
users: [{name: me, password_hash: "$2a$10$abcdefghijklmnopqrstuv"}]
`)
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":8096" || c.ServerName != "JellyBloom" || c.Merge != "union" || c.WebUpstream != "home" || c.DataDir != "data" || c.SecretStore != "file" {
		t.Errorf("defaults = %+v", c)
	}
}

func TestLoadDifference(t *testing.T) {
	c, err := load(t, `
merge: difference
base: home
subtract: [friend]
upstreams:
  - {name: home, url: "http://h:8096"}
  - {name: friend, url: "https://f", user: guest, allow_admin: true, token_command: [op, read, "op://x"]}
users: [{name: me, password_hash: "$2a$10$abcdefghijklmnopqrstuv"}]
`)
	if err != nil {
		t.Fatal(err)
	}
	if c.Base != "home" || len(c.Subtract) != 1 || c.Subtract[0] != "friend" {
		t.Errorf("difference config = %+v", c)
	}
}

func TestLoadErrors(t *testing.T) {
	cases := map[string]string{
		"unknown merge mode":      "merge: xor\nupstreams: [{name: a, url: u}]",
		"no upstreams":            "merge: union",
		"name and url":            "upstreams: [{name: a}]",
		"duplicate upstream name": "upstreams: [{name: a, url: u}, {name: a, url: v}]",
		"secrets no longer go":    "upstreams: [{name: a, url: u, username: me, password: pw}]",
		"field api_key":           "upstreams: [{name: a, url: u, api_key: k}]",
		"field pasword":           "upstreams: [{name: a, url: u, pasword: typo}]",
		"may only contain":        "upstreams: [{name: \"a/b\", url: u}]",
		"go together":             "upstreams: [{name: a, url: u, username: me}]",
		"either token_command":    "upstreams: [{name: a, url: u, username: me, password_file: /p, token_command: [x]}]",
		"requires allow_admin":    "upstreams: [{name: a, url: u, user: x}]",
		"unknown secret_store":    "secret_store: vault\nupstreams: [{name: a, url: u}]",
		"is not a configured":     "web_upstream: b\nupstreams: [{name: a, url: u}]",
		"base \"\" is not":        "merge: difference\nsubtract: [a]\nupstreams: [{name: a, url: u}]",
		"at least one":            "merge: difference\nbase: a\nupstreams: [{name: a, url: u}]",
		"subtract \"z\" is not":   "merge: difference\nbase: a\nsubtract: [z]\nupstreams: [{name: a, url: u}]",
		"both base and subtract":  "merge: difference\nbase: a\nsubtract: [a]\nupstreams: [{name: a, url: u}]",
		"only apply to":           "base: a\nupstreams: [{name: a, url: u}]",
		"at least one user":       "upstreams: [{name: a, url: u}]\nusers: []",
		"duplicate user":          "upstreams: [{name: a, url: u}]\nusers: [{name: Me, password_hash: $2a$x}, {name: me, password_hash: $2a$y}]",
		"must be a bcrypt":        "upstreams: [{name: a, url: u}]\nusers: [{name: me, password_hash: plain}]",
		"fallback_write_allow":    "upstreams: [{name: a, url: u}]\nfallback_write_allow: [\"(\"]",
	}
	for want, yaml := range cases {
		t.Run(want, func(t *testing.T) {
			if !strings.Contains(yaml, "users:") {
				yaml += "\nusers: [{name: me, password_hash: \"$2a$10$abcdefghijklmnopqrstuv\"}]"
			}
			_, err := load(t, yaml)
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("err = %v, want containing %q", err, want)
			}
		})
	}
}
