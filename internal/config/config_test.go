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
  - {name: home, url: "http://h:8096", username: me, password: pw}
  - {name: friend, url: "https://f", api_key: k, user: guest}
`)
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":8096" || c.ServerName != "JellyBloom" || c.Merge != "union" || c.WebUpstream != "home" || c.DataDir != "data" {
		t.Errorf("defaults = %+v", c)
	}
}

func TestLoadDifference(t *testing.T) {
	c, err := load(t, `
merge: difference
base: home
subtract: [friend]
upstreams:
  - {name: home, url: "http://h:8096", username: me, password: pw}
  - {name: friend, url: "https://f", api_key: k, user: guest}
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
		"unknown merge mode":      "merge: xor\nupstreams: [{name: a, url: u, api_key: k, user: x}]",
		"no upstreams":            "merge: union",
		"name and url":            "upstreams: [{name: a, api_key: k, user: x}]",
		"duplicate upstream name": "upstreams: [{name: a, url: u, api_key: k, user: x}, {name: a, url: v, api_key: k, user: x}]",
		"need api_key":            "upstreams: [{name: a, url: u}]",
		"api_key requires user":   "upstreams: [{name: a, url: u, api_key: k}]",
		"is not a configured":     "web_upstream: b\nupstreams: [{name: a, url: u, api_key: k, user: x}]",
		"base \"\" is not":        "merge: difference\nsubtract: [a]\nupstreams: [{name: a, url: u, api_key: k, user: x}]",
		"at least one":            "merge: difference\nbase: a\nupstreams: [{name: a, url: u, api_key: k, user: x}]",
		"subtract \"z\" is not":   "merge: difference\nbase: a\nsubtract: [z]\nupstreams: [{name: a, url: u, api_key: k, user: x}]",
		"both base and subtract":  "merge: difference\nbase: a\nsubtract: [a]\nupstreams: [{name: a, url: u, api_key: k, user: x}]",
		"only apply to":           "base: a\nupstreams: [{name: a, url: u, api_key: k, user: x}]",
	}
	for want, yaml := range cases {
		t.Run(want, func(t *testing.T) {
			_, err := load(t, yaml)
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("err = %v, want containing %q", err, want)
			}
		})
	}
}
