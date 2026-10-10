// Package proxy forwards client requests to upstreams: the generic fallback
// for routes JellyBloom does not implement itself, and the /web UI proxy.
// Bodies stream through memory only; nothing is written to disk.
package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/NebulousOinkler/JellyBloom/internal/identity"
	"github.com/NebulousOinkler/JellyBloom/internal/rewrite"
	"github.com/NebulousOinkler/JellyBloom/internal/upstream"
)

// Resolver is the identity API the proxy needs; *identity.Resolver implements it.
type Resolver interface {
	Lookup(ctx context.Context, id string) (*identity.Unified, error)
	UnifiedID(ctx context.Context, upstream, realID string) (string, error)
}

// Caller is the authenticated client a request is made for. Zero for
// anonymous requests.
type Caller struct {
	UserID string // JellyBloom user ID
	Token  string // JellyBloom token the client used
	Client string // client app name, for logging
}

const (
	maxRewriteBody = 64 << 20 // text responses larger than this are refused
	maxRequestBody = 16 << 20 // JSON request bodies are buffered for rewriting
	copyBuffer     = 64 << 10
)

// Options configures a Proxy.
type Options struct {
	Upstreams   []*upstream.Client // in config order; the first is the primary
	WebUpstream string
	Resolver    Resolver
	ServerID    string // JellyBloom's server ID
	Policy      *Policy
	Transport   http.RoundTripper // default: tuned http.Transport
	// Hidden reports unified IDs hidden by the set operation; list entries
	// for them are dropped from forwarded responses.
	Hidden func(id string) bool
	Logger *slog.Logger
}

// Proxy forwards requests to upstreams. It is safe for concurrent use.
type Proxy struct {
	ups      []*upstream.Client
	byName   map[string]*upstream.Client
	web      *upstream.Client
	res      Resolver
	serverID string
	policy   *Policy
	hidden   func(string) bool
	hc       *http.Client
	log      *slog.Logger

	mu        sync.Mutex
	serverIDs map[string]string // upstream name -> upstream server ID
	seen      map[string]bool   // logged fallback route templates
}

// New creates a Proxy.
func New(o Options) (*Proxy, error) {
	if len(o.Upstreams) == 0 {
		return nil, errors.New("proxy: no upstreams")
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Transport == nil {
		o.Transport = &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConnsPerHost:   32,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 60 * time.Second, // transcode starts can be slow; streams then run untimed
		}
	}
	p := &Proxy{
		ups: o.Upstreams, byName: map[string]*upstream.Client{}, res: o.Resolver, serverID: o.ServerID,
		policy: o.Policy, hidden: o.Hidden, log: o.Logger, serverIDs: map[string]string{}, seen: map[string]bool{},
		// No overall timeout: media streams last as long as playback.
		hc: &http.Client{Transport: o.Transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
	}
	for _, u := range o.Upstreams {
		p.byName[u.Name()] = u
	}
	p.web = p.byName[o.WebUpstream]
	if p.web == nil {
		p.web = o.Upstreams[0]
	}
	if p.policy == nil {
		p.policy, _ = NewPolicy(nil)
	}
	return p, nil
}

// Decide exposes the fallback policy so the server can authenticate first.
func (p *Proxy) Decide(r *http.Request) Decision { return p.policy.Decide(r.Method, r.URL.Path) }

// UpstreamIDs returns the user and server IDs JellyBloom has on an upstream
// ("" when unavailable).
func (p *Proxy) UpstreamIDs(ctx context.Context, name string) (userID, serverID string) {
	up := p.byName[name]
	if up == nil {
		return "", ""
	}
	_, userID, _ = up.Session(ctx)
	return userID, p.upstreamServerID(ctx, up)
}

// upstreamServerID returns (and caches) an upstream's server ID.
func (p *Proxy) upstreamServerID(ctx context.Context, up *upstream.Client) string {
	p.mu.Lock()
	id, ok := p.serverIDs[up.Name()]
	p.mu.Unlock()
	if ok {
		return id
	}
	info, err := up.PublicInfo(ctx)
	if err != nil {
		return ""
	}
	id = identity.NormalizeID(info.Id)
	p.mu.Lock()
	p.serverIDs[up.Name()] = id
	p.mu.Unlock()
	return id
}

// httpError carries a status for the client.
type httpError struct {
	code int
	msg  string
}

func (e *httpError) Error() string { return e.msg }

func fail(w http.ResponseWriter, err error) {
	var he *httpError
	if errors.As(err, &he) {
		http.Error(w, he.msg, he.code)
		return
	}
	http.Error(w, "upstream error", http.StatusBadGateway)
}

// Fallback forwards a request JellyBloom has no handler for to the upstream
// owning the items it references, rewriting IDs, URLs, and tokens both ways.
// The caller must have applied Decide (and authenticated if Authed).
func (p *Proxy) Fallback(w http.ResponseWriter, r *http.Request, c Caller) {
	ctx := r.Context()

	var body []byte
	if r.Body != nil && r.ContentLength != 0 {
		var err error
		body, err = io.ReadAll(io.LimitReader(r.Body, maxRequestBody+1))
		if err != nil {
			http.Error(w, "bad request body", http.StatusBadRequest)
			return
		}
		if len(body) > maxRequestBody {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
	}

	// Collect every GUID the request mentions.
	var guids []string
	collect := func(n string) { guids = append(guids, n) }
	rewrite.EachGUID(r.URL.Path, collect)
	rewrite.EachGUID(r.URL.RawQuery, collect)
	if body != nil {
		rewrite.EachGUID(string(body), collect)
	}

	up, toUpstream, err := p.route(ctx, guids, c)
	if err != nil {
		fail(w, err)
		return
	}
	p.logRoute(r, up, c)

	_, upUser, err := up.Session(ctx)
	if err != nil {
		p.log.Error("upstream login failed", "upstream", up.Name(), "err", err)
		http.Error(w, "upstream unavailable", http.StatusBadGateway)
		return
	}
	req := &rewrite.Replacer{GUID: func(n string) (string, bool) {
		switch n {
		case c.UserID:
			return identity.NormalizeID(upUser), c.UserID != ""
		case p.serverID:
			id := p.upstreamServerID(ctx, up)
			return id, id != ""
		}
		real, ok := toUpstream[n]
		return real, ok
	}}
	path := req.String(r.URL.Path)
	query := req.String(stripTokens(r.URL.Query()).Encode())
	if body != nil {
		body = []byte(req.String(string(body)))
	}
	p.forward(w, r, up, path, query, body, true, c)
}

// route picks the upstream for a request and maps referenced unified IDs to
// that upstream's real IDs. A request about invisible items is refused.
func (p *Proxy) route(ctx context.Context, guids []string, c Caller) (*upstream.Client, map[string]string, error) {
	var refs []*identity.Unified
	for _, g := range guids {
		if g == c.UserID || g == p.serverID {
			continue
		}
		u, err := p.res.Lookup(ctx, g)
		if errors.Is(err, identity.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		if !u.Visible {
			return nil, nil, &httpError{http.StatusNotFound, "item not found"}
		}
		u.ID = g // key by the ID the client sent (Lookup may follow a redirect)
		refs = append(refs, u)
	}

	// Each referenced item constrains the choice to upstreams holding a
	// source copy; pick the first upstream (config order) satisfying all.
	allowed := map[string]bool{}
	for _, u := range p.ups {
		allowed[u.Name()] = true
	}
	var first *upstream.Client
	for _, u := range refs {
		has := map[string]bool{}
		for _, rf := range u.Refs {
			if rf.Source {
				has[rf.Upstream] = true
			}
		}
		next := map[string]bool{}
		for name := range allowed {
			if has[name] {
				next[name] = true
			}
		}
		if first == nil {
			for _, up := range p.ups {
				if has[up.Name()] {
					first = up
					break
				}
			}
		}
		allowed = next
	}
	chosen := first
	for _, up := range p.ups {
		if allowed[up.Name()] {
			chosen = up
			break
		}
	}
	if chosen == nil {
		chosen = p.ups[0] // no item references: primary upstream
	} else if len(allowed) == 0 {
		p.log.Warn("request references items on different upstreams; using the first item's", "upstream", chosen.Name())
	}

	m := map[string]string{}
	for _, u := range refs {
		var real string
		for _, rf := range u.Refs {
			if rf.Upstream == chosen.Name() && (real == "" || rf.Source) {
				real = rf.ID
			}
		}
		if real != "" {
			m[u.ID] = identity.NormalizeID(real)
		}
	}
	return chosen, m, nil
}

// Web forwards /web/* to the web upstream unmodified (the stock jellyfin-web
// UI, including any plugin pages that upstream serves).
func (p *Proxy) Web(w http.ResponseWriter, r *http.Request) {
	p.forward(w, r, p.web, r.URL.Path, stripTokens(r.URL.Query()).Encode(), nil, false, Caller{})
}

// stripTokens removes client token query parameters; the upstream gets its own.
func stripTokens(q url.Values) url.Values {
	for k := range q {
		if strings.EqualFold(k, "api_key") || strings.EqualFold(k, "apikey") {
			delete(q, k)
		}
	}
	return q
}

var hopHeaders = []string{
	"Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "Proxy-Connection",
	"Te", "Trailer", "Transfer-Encoding", "Upgrade",
}

// clientOnlyHeaders never reach the upstream: client credentials and cookies
// for JellyBloom, and Accept-Encoding so the transport negotiates (and
// transparently decodes) compression itself.
var clientOnlyHeaders = []string{
	"Authorization", "X-Emby-Authorization", "X-Emby-Token", "X-MediaBrowser-Token",
	"Cookie", "Accept-Encoding", "Host", "Content-Length",
}

func copyHeaders(dst, src http.Header, skip ...[]string) {
	for k, vs := range src {
		dst[k] = append([]string(nil), vs...)
	}
	for _, list := range skip {
		for _, k := range list {
			dst.Del(k)
		}
	}
	// Headers named by Connection are hop-by-hop too.
	for _, f := range src.Values("Connection") {
		for _, k := range strings.Split(f, ",") {
			dst.Del(strings.TrimSpace(k))
		}
	}
}

// externalBase is JellyBloom's base URL as the client reached it.
func externalBase(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if fp := r.Header.Get("X-Forwarded-Proto"); fp != "" {
		scheme = strings.TrimSpace(strings.Split(fp, ",")[0])
	}
	host := r.Host
	if fh := r.Header.Get("X-Forwarded-Host"); fh != "" {
		host = strings.TrimSpace(strings.Split(fh, ",")[0])
	}
	return scheme + "://" + host
}

func (p *Proxy) forward(w http.ResponseWriter, r *http.Request, up *upstream.Client, path, query string, body []byte, rewriteResp bool, c Caller) {
	ctx := r.Context()
	token, upUser, err := up.Session(ctx)
	if err != nil {
		p.log.Error("upstream login failed", "upstream", up.Name(), "err", err)
		http.Error(w, "upstream unavailable", http.StatusBadGateway)
		return
	}
	target := up.BaseURL()
	target.Path += path
	target.RawPath = ""
	target.RawQuery = query

	send := func(token string) (*http.Response, error) {
		var rb io.Reader
		switch {
		case body != nil:
			rb = bytes.NewReader(body)
		case r.Body != nil && r.ContentLength != 0:
			rb = r.Body // streamed (only when not buffered for rewriting)
		}
		req, err := http.NewRequestWithContext(ctx, r.Method, target.String(), rb)
		if err != nil {
			return nil, err
		}
		copyHeaders(req.Header, r.Header, hopHeaders, clientOnlyHeaders)
		req.Header.Set("Authorization", up.AuthHeader(token))
		if body != nil {
			req.ContentLength = int64(len(body))
		} else if rb != nil {
			req.ContentLength = r.ContentLength
		}
		return p.hc.Do(req)
	}

	resp, err := send(token)
	replayable := body != nil || r.Body == nil || r.ContentLength == 0
	if err == nil && resp.StatusCode == http.StatusUnauthorized && replayable {
		resp.Body.Close()
		if err = up.Reauth(ctx, token); err == nil {
			token, upUser, err = up.Session(ctx)
			if err == nil {
				resp, err = send(token)
			}
		}
	}
	if err != nil {
		if ctx.Err() == nil {
			p.log.Warn("upstream request failed", "upstream", up.Name(), "path", rewrite.Template(path), "err", err)
			http.Error(w, "upstream unavailable", http.StatusBadGateway)
		}
		return
	}
	defer resp.Body.Close()

	// Even unmodified responses must not leak the upstream's address in redirects.
	rep := &rewrite.Replacer{Literals: []string{up.BaseURL().String(), externalBase(r)}}
	if rewriteResp {
		rep = p.responseReplacer(ctx, r, up, token, upUser, c)
	}
	copyHeaders(w.Header(), resp.Header, hopHeaders, []string{"Set-Cookie"})
	if loc := resp.Header.Get("Location"); loc != "" {
		w.Header().Set("Location", rep.String(loc))
	}

	ct := resp.Header.Get("Content-Type")
	if !rewriteResp || !rewrite.IsText(ct) {
		w.WriteHeader(resp.StatusCode)
		streamBody(w, resp.Body)
		return
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxRewriteBody+1))
	if err != nil {
		if ctx.Err() == nil {
			http.Error(w, "upstream read failed", http.StatusBadGateway)
		}
		return
	}
	if len(raw) > maxRewriteBody {
		http.Error(w, "upstream response too large", http.StatusBadGateway)
		return
	}
	out := rep.Text(raw)
	if rewrite.IsJSON(ct) {
		out = p.dropHidden(rep.JSON(raw))
	}
	h := w.Header()
	for _, k := range []string{"Content-Length", "Etag", "Content-Md5", "Accept-Ranges", "Content-Range"} {
		h.Del(k)
	}
	h.Set("Content-Length", fmt.Sprint(len(out)))
	w.WriteHeader(resp.StatusCode)
	w.Write(out)
}

// responseReplacer maps upstream IDs, URLs, and tokens to JellyBloom's.
func (p *Proxy) responseReplacer(ctx context.Context, r *http.Request, up *upstream.Client, token, upUser string, c Caller) *rewrite.Replacer {
	upUser = identity.NormalizeID(upUser)
	upServer := p.upstreamServerID(ctx, up)
	upToken := identity.NormalizeID(token)
	memo := map[string]string{}
	base := up.BaseURL().String()
	return &rewrite.Replacer{
		GUID: func(n string) (string, bool) {
			switch {
			case n == upToken:
				return "", false // replaced by the literal below
			case n == upUser && c.UserID != "":
				return c.UserID, true
			case n == upServer:
				return p.serverID, true
			}
			if id, ok := memo[n]; ok {
				return id, true
			}
			id, err := p.res.UnifiedID(ctx, up.Name(), n)
			if err != nil {
				p.log.Error("mapping upstream id", "upstream", up.Name(), "err", err)
				return "", false
			}
			memo[n] = id
			return id, true
		},
		Literals: []string{base, externalBase(r), token, c.Token},
	}
}

// streamBody relays a body through a fixed buffer, flushing as data arrives
// so live streams and HLS start immediately.
func streamBody(w http.ResponseWriter, body io.Reader) {
	rc := http.NewResponseController(w)
	buf := make([]byte, copyBuffer)
	for {
		n, err := body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			rc.Flush()
		}
		if err != nil {
			return
		}
	}
}

// logRoute records each distinct fallback route once, to show which routes
// deserve dedicated handlers.
func (p *Proxy) logRoute(r *http.Request, up *upstream.Client, c Caller) {
	key := r.Method + " " + strings.ToLower(rewrite.Template(r.URL.Path))
	p.mu.Lock()
	first := !p.seen[key]
	p.seen[key] = true
	p.mu.Unlock()
	if first {
		p.log.Info("fallback route", "route", key, "upstream", up.Name(), "client", c.Client)
	}
}

// dropHidden removes items hidden by the set operation from list responses
// (a {"Items": [...]} envelope or a bare array of items), so fallback routes
// such as /Items/{id}/Similar cannot reveal them.
func (p *Proxy) dropHidden(body []byte) []byte {
	if p.hidden == nil {
		return body
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || (trimmed[0] != '{' && trimmed[0] != '[') {
		return body
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var v any
	if dec.Decode(&v) != nil {
		return body
	}
	filter := func(list []any) ([]any, int) {
		out := list[:0]
		removed := 0
		for _, e := range list {
			if m, ok := e.(map[string]any); ok {
				if id, ok := m["Id"].(string); ok && p.hidden(id) {
					removed++
					continue
				}
			}
			out = append(out, e)
		}
		return out, removed
	}
	changed := false
	switch t := v.(type) {
	case []any:
		var n int
		if v, n = filter(t); n > 0 {
			changed = true
		}
	case map[string]any:
		if list, ok := t["Items"].([]any); ok {
			kept, n := filter(list)
			if n > 0 {
				changed = true
				t["Items"] = kept
				if total, ok := t["TotalRecordCount"].(json.Number); ok {
					if c, err := total.Int64(); err == nil {
						t["TotalRecordCount"] = max(c-int64(n), 0)
					}
				}
			}
		}
	}
	if !changed {
		return body
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if enc.Encode(v) != nil {
		return body
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
}
