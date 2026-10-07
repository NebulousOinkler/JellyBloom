// Package config loads JellyBloom's YAML configuration.
package config

import (
	"fmt"
	"os"

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

type Config struct {
	Listen      string     `yaml:"listen"`
	ServerName  string     `yaml:"server_name"`
	Merge       string     `yaml:"merge"`
	WebUpstream string     `yaml:"web_upstream"`
	DataDir     string     `yaml:"data_dir"`
	Upstreams   []Upstream `yaml:"upstreams"`
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
	if c.WebUpstream == "" {
		c.WebUpstream = c.Upstreams[0].Name
	} else if !names[c.WebUpstream] {
		return nil, fmt.Errorf("web_upstream %q is not a configured upstream", c.WebUpstream)
	}
	return c, nil
}
