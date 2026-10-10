package proxy

import (
	"net/http"
	"regexp"
	"strings"

	"github.com/NebulousOinkler/JellyBloom/internal/rewrite"
)

// Decision is the fallback policy's verdict on a request.
type Decision int

const (
	Deny      Decision = iota // 403: admin or unlisted write route
	Anonymous                 // forward without a JellyBloom session
	Authed                    // forward with a JellyBloom session
)

// deniedPrefixes are upstream admin/system surfaces. JellyBloom forwards with
// its own upstream credentials, which may be an admin API key, so these are
// blocked for every client.
var deniedPrefixes = []string{
	"/system", "/plugins", "/packages", "/repositories", "/scheduledtasks", "/startup",
	"/environment", "/auth/", "/devices", "/notifications", "/activitylog", "/backup",
	"/library/virtualfolders", "/library/physicalpaths", "/library/refresh",
	"/library/media/updated", "/library/movies/", "/library/series/", "/livetv",
	"/users/new", "/users/forgotpassword", "/quickconnect", "/syncplay", "/tmdb", "/encoding",
}

// deniedExact are exact paths (lowercase, GUIDs templated) that are blocked.
var deniedExact = map[string]bool{"/users": true, "/sessions": true}

var builtinWriteAllow = []*regexp.Regexp{
	regexp.MustCompile(`^/sessions/playing(/progress|/stopped|/ping)?$`),
	regexp.MustCompile(`^/items/\{id\}/playbackinfo$`),
	regexp.MustCompile(`^/livestreams/(open|close)$`),
	regexp.MustCompile(`^/videos/activeencodings$`),
	regexp.MustCompile(`^/(userplayeditems|userfavoriteitems)/\{id\}$`),
	regexp.MustCompile(`^/useritems/\{id\}/(rating|userdata)$`),
	regexp.MustCompile(`^/users/\{id\}/(playeditems|favoriteitems|playingitems)/\{id\}(/progress)?$`),
	regexp.MustCompile(`^/users/\{id\}/items/\{id\}/(rating|userdata)$`),
}

// Policy decides which fallback requests may be forwarded.
type Policy struct {
	writeAllow []*regexp.Regexp
}

// NewPolicy adds extra write-allow regexes (from config) to the built-ins.
func NewPolicy(extra []string) (*Policy, error) {
	p := &Policy{writeAllow: builtinWriteAllow}
	for _, e := range extra {
		re, err := regexp.Compile(e)
		if err != nil {
			return nil, err
		}
		p.writeAllow = append(p.writeAllow, re)
	}
	return p, nil
}

// Decide classifies a request by method and path.
func (p *Policy) Decide(method, path string) Decision {
	t := strings.TrimSuffix(strings.ToLower(rewrite.Template(path)), "/")
	if t == "" {
		t = "/"
	}
	if deniedExact[t] {
		return Deny
	}
	for _, d := range deniedPrefixes {
		// "/system" denies /system and /system/…, not /systemfoo; "/auth/"
		// denies only below /auth.
		if strings.HasSuffix(d, "/") && strings.HasPrefix(t, d) || t == d || strings.HasPrefix(t, d+"/") {
			return Deny
		}
	}
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		// Jellyfin serves artwork and localization without authentication.
		if strings.Contains(t, "/images") || strings.HasPrefix(t, "/localization/") {
			return Anonymous
		}
		return Authed
	}
	for _, re := range p.writeAllow {
		if re.MatchString(t) {
			return Authed
		}
	}
	return Deny
}
