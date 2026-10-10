package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"golang.org/x/crypto/bcrypt"

	"github.com/NebulousOinkler/JellyBloom/internal/auth"
	"github.com/NebulousOinkler/JellyBloom/internal/catalog"
	"github.com/NebulousOinkler/JellyBloom/internal/config"
	"github.com/NebulousOinkler/JellyBloom/internal/identity"
	"github.com/NebulousOinkler/JellyBloom/internal/index"
	"github.com/NebulousOinkler/JellyBloom/internal/playback"
	"github.com/NebulousOinkler/JellyBloom/internal/proxy"
	"github.com/NebulousOinkler/JellyBloom/internal/upstream"
)

func hexID(prefix string) string { return prefix + strings.Repeat("0", 32-len(prefix)) }

var (
	shared  = map[string]string{"a": hexID("a1"), "b": hexID("b1")} // same movie on both
	aOnly   = hexID("a2")
	bOnly   = hexID("b2")
	unknown = hexID("ee") // upstream GUID that is not an indexed item
	tagHex  = hexID("7a")
)

// fakeUpstream is a Jellyfin server that records requests and echoes them.
type fakeUpstream struct {
	name    string
	srv     *httptest.Server
	userID  string
	server  string
	mu      sync.Mutex
	logins  int
	token   string
	valid   map[string]bool // issued, unrevoked tokens
	last    *http.Request
	lastRaw []byte
	history []recorded
	media   map[string][]map[string]any // real item ID -> PlaybackInfo media sources
}

type recorded struct {
	method, path string
	query        url.Values
	body, auth   string
}

var deviceRe = regexp.MustCompile(`DeviceId="([^"]*)"`)

// find returns recorded requests whose path ends with suffix.
func (f *fakeUpstream) find(suffix string) []recorded {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []recorded
	for _, r := range f.history {
		if strings.HasSuffix(strings.ToLower(r.path), strings.ToLower(suffix)) {
			out = append(out, r)
		}
	}
	return out
}

func newFakeUpstream(t *testing.T, name string) *fakeUpstream {
	f := &fakeUpstream{name: name, userID: hexID("f" + name), server: hexID("5" + name), valid: map[string]bool{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

var tokenRe = regexp.MustCompile(`Token="([^"]*)"`)

func (f *fakeUpstream) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	if r.URL.Path != "/Users/AuthenticateByName" && r.URL.Path != "/System/Info/Public" {
		f.mu.Lock()
		f.last, f.lastRaw = r, body
		f.history = append(f.history, recorded{r.Method, r.URL.Path, r.URL.Query(), string(body), r.Header.Get("Authorization")})
		f.mu.Unlock()
	}

	switch {
	case r.URL.Path == "/Users/AuthenticateByName":
		f.mu.Lock()
		f.logins++
		f.token = hexID(fmt.Sprintf("%s%d", "c"+f.name, f.logins))
		f.valid[f.token] = true
		tok := f.token
		f.mu.Unlock()
		writeJSON(w, map[string]any{"AccessToken": tok, "User": map[string]any{"Id": f.userID}})
		return
	case r.URL.Path == "/System/Info/Public":
		writeJSON(w, map[string]any{"Id": f.server, "Version": "10.9.11"})
		return
	case strings.HasPrefix(r.URL.Path, "/web/"):
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, "<html>web from %s %s</html>", f.name, aOnly)
		return
	}

	m := tokenRe.FindStringSubmatch(r.Header.Get("Authorization"))
	f.mu.Lock()
	ok := m != nil && f.valid[m[1]]
	f.mu.Unlock()
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	tok := m[1]
	device := ""
	if d := deviceRe.FindStringSubmatch(r.Header.Get("Authorization")); d != nil {
		device = d[1]
	}
	q := r.URL.Query()
	switch {
	case strings.HasSuffix(r.URL.Path, "/PlaybackInfo"):
		item := strings.Split(r.URL.Path, "/")[2]
		want := q.Get("MediaSourceId")
		var sources []map[string]any
		for _, ms := range f.media[item] {
			if want != "" && ms["Id"] != want {
				continue
			}
			c := map[string]any{}
			for k, v := range ms {
				c[k] = v
			}
			c["TranscodingUrl"] = fmt.Sprintf("/videos/%s/master.m3u8?DeviceId=%s&MediaSourceId=%s&PlaySessionId=%s&ApiKey=%s",
				item, device, ms["Id"], hexID("9"+f.name), tok)
			sources = append(sources, c)
		}
		writeJSON(w, map[string]any{"MediaSources": sources, "PlaySessionId": hexID("9" + f.name)})
		return
	case strings.HasSuffix(r.URL.Path, "/master.m3u8"):
		// Like Jellyfin, variant URLs carry the request's own ApiKey, if any.
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		key := ""
		if k := q.Get("ApiKey"); k != "" {
			key = "&ApiKey=" + k
		}
		fmt.Fprintf(w, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1000\nmain.m3u8?DeviceId=%s&MediaSourceId=%s&PlaySessionId=%s%s\n",
			q.Get("DeviceId"), q.Get("MediaSourceId"), q.Get("PlaySessionId"), key)
		return
	case strings.Contains(r.URL.Path, "/hls1/"):
		w.Header().Set("Content-Type", "video/mp4")
		w.Write([]byte("SEGMENT " + f.name))
		return
	}
	if strings.HasSuffix(r.URL.Path, "/stream") {
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("X-Got-Range", r.Header.Get("Range"))
		w.WriteHeader(http.StatusPartialContent)
		w.Write([]byte("BYTES " + aOnly + " " + tok))
		return
	}
	if r.URL.Query().Get("list") == "1" {
		writeJSON(w, map[string]any{"Items": []map[string]any{{"Id": shared[f.name]}, {"Id": aOnly}, {"Id": bOnly}}, "TotalRecordCount": 3})
		return
	}
	writeJSON(w, map[string]any{
		"Upstream":  f.name,
		"Path":      r.URL.Path,
		"Query":     r.URL.RawQuery,
		"Body":      string(body),
		"User":      f.userID,
		"Server":    f.server,
		"Url":       f.srv.URL + "/Videos/" + aOnly + "/stream?api_key=" + tok,
		"ImageTags": map[string]string{"Primary": tagHex},
		"Unknown":   unknown,
	})
}

func (f *fakeUpstream) lastRequest() (*http.Request, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.last, string(f.lastRaw)
}

func (f *fakeUpstream) revoke() {
	f.mu.Lock()
	f.valid = map[string]bool{}
	f.mu.Unlock()
}

type env struct {
	t        *testing.T
	srv      *httptest.Server
	server   *Server
	res      *identity.Resolver
	auth     *auth.Service
	a, b     *fakeUpstream
	token    string
	userID   string
	serverID string
}

// library is one upstream's indexed content: libraries and their items.
type library struct {
	views []map[string]any
	items map[string][]map[string]any // view ID -> items
}

var defaultData = map[string]library{
	"a": {
		views: []map[string]any{{"Id": hexID("va"), "Name": "Movies", "CollectionType": "movies"}},
		items: map[string][]map[string]any{hexID("va"): {
			{"Id": shared["a"], "Type": "Movie", "Name": "Shared", "ParentId": hexID("va"), "ProviderIds": map[string]string{"Tmdb": "1"}},
			{"Id": aOnly, "Type": "Movie", "Name": "A", "ParentId": hexID("va"), "ProviderIds": map[string]string{"Tmdb": "2"}},
		}},
	},
	"b": {
		views: []map[string]any{{"Id": hexID("vb"), "Name": "Movies", "CollectionType": "movies"}},
		items: map[string][]map[string]any{hexID("vb"): {
			{"Id": shared["b"], "Type": "Movie", "Name": "Shared", "ParentId": hexID("vb"), "ProviderIds": map[string]string{"Tmdb": "1"}},
			{"Id": bOnly, "Type": "Movie", "Name": "B", "ParentId": hexID("vb"), "ProviderIds": map[string]string{"Tmdb": "3"}},
		}},
	},
}

func newEnv(t *testing.T, mode identity.Mode) *env { return newEnvWith(t, mode, defaultData) }

func newEnvWith(t *testing.T, mode identity.Mode, data map[string]library) *env {
	t.Helper()
	ctx := context.Background()
	idx, err := index.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { idx.Close() })

	e := &env{t: t, a: newFakeUpstream(t, "a"), b: newFakeUpstream(t, "b")}
	raws := func(ds []map[string]any) []json.RawMessage {
		var out []json.RawMessage
		for _, d := range ds {
			b, _ := json.Marshal(d)
			out = append(out, b)
		}
		return out
	}
	for up, lib := range data {
		fs, _ := idx.BeginFullSync(ctx, up)
		if err := fs.PutViews(ctx, raws(lib.views)); err != nil {
			t.Fatal(err)
		}
		for v, items := range lib.items {
			if err := fs.PutItems(ctx, v, raws(items)); err != nil {
				t.Fatal(err)
			}
		}
		fs.Commit(ctx)
	}
	mode.Upstreams = []string{"a", "b"}
	if mode.Op == "" {
		mode.Op = identity.Union
	}
	if e.res, err = identity.New(idx, mode, nil); err != nil {
		t.Fatal(err)
	}

	h, _ := bcrypt.GenerateFromPassword([]byte("pw"), bcrypt.MinCost)
	if e.auth, err = auth.New(idx.DB(), []config.User{{Name: "me", PasswordHash: string(h)}}); err != nil {
		t.Fatal(err)
	}
	var clients []*upstream.Client
	for _, f := range []*fakeUpstream{e.a, e.b} {
		c, err := upstream.New(config.Upstream{Name: f.name, URL: f.srv.URL, Username: "u", Password: "p"}, upstream.Options{})
		if err != nil {
			t.Fatal(err)
		}
		clients = append(clients, c)
	}
	var cat *catalog.Catalog
	policy, _ := proxy.NewPolicy(nil)
	px, err := proxy.New(proxy.Options{Upstreams: clients, WebUpstream: "b", Resolver: e.res, ServerID: e.auth.ServerID(),
		Policy: policy, Hidden: func(id string) bool { return cat.Hidden(id) }})
	if err != nil {
		t.Fatal(err)
	}
	cat = catalog.New(catalog.Options{DB: idx.DB(), Resolver: e.res, Upstreams: []string{"a", "b"},
		ServerID: e.auth.ServerID(), UpstreamIDs: px.UpstreamIDs})
	e.res.OnRebuild(func(ctx context.Context) {
		if err := cat.Reload(ctx); err != nil {
			t.Error(err)
		}
	})
	if _, err := e.res.Rebuild(ctx); err != nil {
		t.Fatal(err)
	}
	e.server, err = New(Options{ServerName: "Bloom", DB: idx.DB(), Auth: e.auth, Proxy: px, Catalog: cat,
		Playback: playback.New(px, e.res, nil),
		Version: func(ctx context.Context) (string, error) {
			info, err := clients[1].PublicInfo(ctx)
			if err != nil {
				return "", err
			}
			return info.Version, nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	e.srv = httptest.NewServer(e.server)
	t.Cleanup(e.srv.Close)
	e.serverID = e.auth.ServerID()
	return e
}

func (e *env) uid(upstream, real string) string {
	e.t.Helper()
	id, err := e.res.UnifiedID(context.Background(), upstream, real)
	if err != nil {
		e.t.Fatal(err)
	}
	return id
}

type resp struct {
	code   int
	header http.Header
	body   []byte
}

func (r resp) json(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(r.body, &m); err != nil {
		t.Fatalf("not JSON (%d): %s", r.code, r.body)
	}
	return m
}

func (e *env) do(method, path, token string, body any, hdr ...string) resp {
	e.t.Helper()
	var rb io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rb = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, e.srv.URL+path, rb)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", `MediaBrowser Client="Test", Device="T", DeviceId="dev1", Version="1"`+
		map[bool]string{true: `, Token="` + token + `"`, false: ""}[token != ""])
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := client.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return resp{res.StatusCode, res.Header, b}
}

func (e *env) login() {
	e.t.Helper()
	r := e.do("POST", "/users/authenticatebyname", "", map[string]string{"Username": "me", "Pw": "pw"})
	if r.code != 200 {
		e.t.Fatalf("login: %d %s", r.code, r.body)
	}
	m := r.json(e.t)
	e.token = m["AccessToken"].(string)
	e.userID = m["User"].(map[string]any)["Id"].(string)
	if m["ServerId"] != e.serverID {
		e.t.Errorf("ServerId = %v", m["ServerId"])
	}
}

func TestServerIdentityAndAuth(t *testing.T) {
	e := newEnv(t, identity.Mode{})

	info := e.do("GET", "/system/info/public", "", nil).json(t)
	if info["Id"] != e.serverID || info["ProductName"] != "Jellyfin Server" || info["ServerName"] != "Bloom" || info["Version"] != "10.9.11" {
		t.Errorf("public info = %v", info)
	}
	if r := e.do("GET", "/System/Info", "", nil); r.code != 401 {
		t.Errorf("System/Info without token = %d", r.code)
	}
	if r := e.do("POST", "/Users/AuthenticateByName", "", map[string]string{"Username": "me", "Pw": "bad"}); r.code != 401 {
		t.Errorf("bad password = %d", r.code)
	}

	e.login()
	if r := e.do("GET", "/System/Info", e.token, nil); r.code != 200 {
		t.Errorf("System/Info = %d", r.code)
	}
	me := e.do("GET", "/Users/Me", e.token, nil).json(t)
	if me["Id"] != e.userID || me["Policy"].(map[string]any)["IsAdministrator"] != false {
		t.Errorf("Users/Me = %v", me)
	}
	if r := e.do("GET", "/Users/"+hexID("99"), e.token, nil); r.code != 404 {
		t.Errorf("other user = %d", r.code)
	}

	// Token in X-Emby-Token and api_key works too.
	if r := e.do("GET", "/System/Info?api_key="+e.token, "", nil); r.code != 200 {
		t.Errorf("api_key auth = %d", r.code)
	}

	if r := e.do("POST", "/Sessions/Logout", e.token, nil); r.code != 204 {
		t.Errorf("logout = %d", r.code)
	}
	if r := e.do("GET", "/Users/Me", e.token, nil); r.code != 401 {
		t.Errorf("after logout = %d", r.code)
	}
}

func TestPreferences(t *testing.T) {
	e := newEnv(t, identity.Mode{})
	e.login()

	if r := e.do("POST", "/DisplayPreferences/usersettings?client=emby", e.token, map[string]any{"Id": "usersettings", "CustomPrefs": map[string]string{"x": "1"}}); r.code != 204 {
		t.Fatalf("save prefs = %d", r.code)
	}
	got := e.do("GET", "/displaypreferences/usersettings?client=emby", e.token, nil).json(t)
	if got["CustomPrefs"].(map[string]any)["x"] != "1" {
		t.Errorf("prefs = %v", got)
	}
	if def := e.do("GET", "/DisplayPreferences/other?client=emby", e.token, nil).json(t); def["SortBy"] != "SortName" {
		t.Errorf("default prefs = %v", def)
	}

	if r := e.do("POST", "/Users/"+e.userID+"/Configuration", e.token, map[string]any{"SubtitleMode": "Always"}); r.code != 204 {
		t.Fatalf("save config = %d", r.code)
	}
	cfg := e.do("GET", "/Users/Me", e.token, nil).json(t)["Configuration"].(map[string]any)
	if cfg["SubtitleMode"] != "Always" || cfg["PlayDefaultAudioTrack"] != true {
		t.Errorf("config = %v", cfg)
	}
}

func TestFallbackRoutingAndRewriting(t *testing.T) {
	e := newEnv(t, identity.Mode{})
	e.login()
	uA := e.uid("a", aOnly)

	r := e.do("GET", "/Items/"+uA+"/Similar?userId="+e.userID, e.token, nil)
	if r.code != 200 {
		t.Fatalf("fallback = %d %s", r.code, r.body)
	}
	got := r.json(t)
	upReq, _ := e.a.lastRequest()
	if got["Upstream"] != "a" || upReq.URL.Path != "/Items/"+aOnly+"/Similar" {
		t.Errorf("routed to %v with path %s", got["Upstream"], upReq.URL.Path)
	}
	if q := upReq.URL.Query().Get("userId"); q != e.a.userID {
		t.Errorf("userId not mapped to upstream user: %q", q)
	}
	// The upstream sees the client's app as its own device (so sessions and
	// transcodes of different clients stay apart), never JellyBloom's token or
	// the client's raw device ID.
	auth := upReq.Header.Get("Authorization")
	wantDevice := upstream.DeviceID("a", e.userID, "dev1")
	if strings.Contains(auth, e.token) || strings.Contains(auth, `"dev1"`) || !strings.Contains(auth, `DeviceId="`+wantDevice+`"`) {
		t.Errorf("upstream auth = %q", auth)
	}

	// The response carries only JellyBloom IDs, URLs, and tokens.
	if got["User"] != e.userID || got["Server"] != e.serverID {
		t.Errorf("user/server not rewritten: %v %v", got["User"], got["Server"])
	}
	wantURL := e.srv.URL + "/Videos/" + uA + "/stream?api_key=" + e.token
	if got["Url"] != wantURL {
		t.Errorf("Url = %v, want %v", got["Url"], wantURL)
	}
	if got["ImageTags"].(map[string]any)["Primary"] != tagHex {
		t.Error("image tag rewritten")
	}
	for _, leak := range []string{e.a.srv.URL, aOnly, e.a.userID, e.a.server, unknown} {
		if bytes.Contains(r.body, []byte(leak)) {
			t.Errorf("response leaks %q: %s", leak, r.body)
		}
	}

	// The unknown upstream GUID became a lazy ID that routes back to a.
	// (Echoed paths are rewritten back to unified IDs, so check what the
	// upstream actually received.)
	lazy := got["Unknown"].(string)
	e.do("GET", "/Foo/"+lazy, e.token, nil)
	if req, _ := e.a.lastRequest(); req.URL.Path != "/Foo/"+unknown {
		t.Errorf("lazy id did not route back: upstream got %s", req.URL.Path)
	}

	// b-only item goes to b; shared item goes to the primary upstream (a).
	if got := e.do("GET", "/Items/"+e.uid("b", bOnly)+"/Similar", e.token, nil).json(t); got["Upstream"] != "b" {
		t.Errorf("b-only routed to %v", got["Upstream"])
	}
	if req, _ := e.b.lastRequest(); req.URL.Path != "/Items/"+bOnly+"/Similar" {
		t.Errorf("b got %s", req.URL.Path)
	}
	if got := e.do("GET", "/Items/"+e.uid("b", shared["b"])+"/Similar", e.token, nil).json(t); got["Upstream"] != "a" {
		t.Errorf("shared item routed to %v", got["Upstream"])
	}
	if req, _ := e.a.lastRequest(); req.URL.Path != "/Items/"+shared["a"]+"/Similar" {
		t.Errorf("a got %s for the shared item", req.URL.Path)
	}

	// POST body IDs are rewritten too.
	e.do("POST", "/Sessions/Playing", e.token, map[string]any{"ItemId": e.uid("b", bOnly)})
	if _, body := e.b.lastRequest(); !strings.Contains(body, bOnly) {
		t.Errorf("playing report body = %s", body)
	}

	// Expired upstream token: re-login and retry transparently.
	e.a.revoke()
	if r := e.do("GET", "/Items/"+uA+"/Similar", e.token, nil); r.code != 200 {
		t.Errorf("after upstream token revoked = %d", r.code)
	}
}

func TestFallbackPolicy(t *testing.T) {
	e := newEnv(t, identity.Mode{})
	e.login()
	uA := e.uid("a", aOnly)

	for _, c := range []struct {
		method, path, token string
		want                int
	}{
		{"GET", "/System/Logs", e.token, 403},
		{"GET", "/Auth/Keys", e.token, 403},
		{"DELETE", "/Items/" + uA, e.token, 403},
		{"POST", "/Library/Refresh", e.token, 403},
		{"GET", "/Items/" + uA + "/Similar", "", 401},
		{"GET", "/Items/" + uA + "/Images/Primary", "", 200},
	} {
		if r := e.do(c.method, c.path, c.token, nil); r.code != c.want {
			t.Errorf("%s %s = %d, want %d", c.method, c.path, r.code, c.want)
		}
	}
}

func TestStreamsPassThroughUnmodified(t *testing.T) {
	e := newEnv(t, identity.Mode{})
	e.login()
	r := e.do("GET", "/Videos/"+e.uid("a", aOnly)+"/stream", e.token, nil, "Range", "bytes=0-3")
	if r.code != 206 || r.header.Get("X-Got-Range") != "bytes=0-3" {
		t.Errorf("stream = %d, range %q", r.code, r.header.Get("X-Got-Range"))
	}
	if !strings.HasPrefix(string(r.body), "BYTES "+aOnly) {
		t.Errorf("binary body was modified: %q", r.body)
	}
}

func TestDifferenceHidesItems(t *testing.T) {
	e := newEnv(t, identity.Mode{Op: identity.Difference, Base: "a", Subtract: []string{"b"}})
	e.login()
	if r := e.do("GET", "/Items/"+e.uid("b", bOnly)+"/Similar", e.token, nil); r.code != 404 {
		t.Errorf("b-only item under a\\b = %d", r.code)
	}
	if r := e.do("GET", "/Items/"+e.uid("a", shared["a"])+"/Similar", e.token, nil); r.code != 404 {
		t.Errorf("shared item under a\\b = %d", r.code)
	}
	if r := e.do("GET", "/Items/"+e.uid("a", aOnly)+"/Similar", e.token, nil); r.code != 200 {
		t.Errorf("a-only item under a\\b = %d", r.code)
	}
}

func TestWebProxy(t *testing.T) {
	e := newEnv(t, identity.Mode{})
	if r := e.do("GET", "/", "", nil); r.code != 302 || r.header.Get("Location") != "/web/" {
		t.Errorf("root = %d %s", r.code, r.header.Get("Location"))
	}
	if r := e.do("GET", "/web", "", nil); r.code != 302 || r.header.Get("Location") != "/web/" {
		t.Errorf("/web = %d %s", r.code, r.header.Get("Location"))
	}
	if r := e.do("GET", "/web/", "", nil); r.code != 200 {
		t.Errorf("/web/ = %d %s, want the web UI (not a redirect loop)", r.code, r.header.Get("Location"))
	}
	r := e.do("GET", "/web/index.html", "", nil)
	if r.code != 200 || string(r.body) != "<html>web from b "+aOnly+"</html>" {
		t.Errorf("web = %d %q (must come unmodified from web_upstream b)", r.code, r.body)
	}
}

func TestSocket(t *testing.T) {
	e := newEnv(t, identity.Mode{})
	e.login()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(e.srv.URL, "http")+"/socket?api_key=bad", nil); err == nil {
		t.Error("socket accepted a bad token")
	}
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(e.srv.URL, "http")+"/socket?api_key="+e.token+"&deviceId=dev1", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()

	var msg struct {
		MessageType string
		Data        json.RawMessage
	}
	if err := wsjson.Read(ctx, conn, &msg); err != nil || msg.MessageType != "ForceKeepAlive" {
		t.Fatalf("first message = %+v, %v", msg, err)
	}
	wsjson.Write(ctx, conn, map[string]string{"MessageType": "KeepAlive"})
	if err := wsjson.Read(ctx, conn, &msg); err != nil || msg.MessageType != "KeepAlive" {
		t.Fatalf("keepalive reply = %+v, %v", msg, err)
	}

	for e.server.hub.count() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	e.server.Broadcast("LibraryChanged", map[string]any{"ItemsAdded": []string{"x"}})
	if err := wsjson.Read(ctx, conn, &msg); err != nil || msg.MessageType != "LibraryChanged" {
		t.Fatalf("broadcast = %+v, %v", msg, err)
	}
}

func TestRouterCaseInsensitive(t *testing.T) {
	rt := &router{fallback: http.NotFoundHandler()}
	var got string
	rt.handle("GET", "/Users/{userId}/Items", func(w http.ResponseWriter, r *http.Request) { got = r.PathValue("userId") })
	rt.handle("", "/web/{rest...}", func(w http.ResponseWriter, r *http.Request) { got = r.PathValue("rest") })

	for path, want := range map[string]string{
		"/users/AbC/items": "AbC", "/USERS/x/ITEMS/": "x", "/Web/a/b.js": "a/b.js",
	} {
		got = ""
		rec := httptest.NewRecorder()
		rt.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if got != want {
			t.Errorf("%s: param = %q, want %q (code %d)", path, got, want, rec.Code)
		}
	}
	rec := httptest.NewRecorder()
	rt.ServeHTTP(rec, httptest.NewRequest("POST", "/Users/x/Items", nil))
	if rec.Code != 404 {
		t.Errorf("wrong method should reach fallback, got %d", rec.Code)
	}
}

func TestFallbackListsDropHiddenItems(t *testing.T) {
	e := newEnv(t, identity.Mode{Op: identity.Difference, Base: "a", Subtract: []string{"b"}})
	e.login()
	got := e.do("GET", "/Items/"+e.uid("a", aOnly)+"/Similar?list=1", e.token, nil).json(t)
	items := got["Items"].([]any)
	if len(items) != 2 || got["TotalRecordCount"] != float64(2) {
		t.Fatalf("list = %v", got)
	}
	for _, it := range items {
		if id := it.(map[string]any)["Id"]; id == e.uid("a", shared["a"]) {
			t.Errorf("hidden shared item leaked through fallback list: %v", got)
		}
	}
}

// TestMilestoneMusicAndVideo is phase 5's milestone: a music-only upstream and
// a video-only upstream browse as one server.
func TestMilestoneMusicAndVideo(t *testing.T) {
	music, movies := hexID("a0"), hexID("b0")
	artistID, albumID, trackID, movieID := hexID("a3"), hexID("a4"), hexID("a5"), hexID("b3")
	e := newEnvWith(t, identity.Mode{}, map[string]library{
		"a": {
			views: []map[string]any{{"Id": music, "Name": "Music", "CollectionType": "music", "ImageTags": map[string]string{"Primary": tagHex}}},
			items: map[string][]map[string]any{music: {
				{"Id": artistID, "Type": "MusicArtist", "Name": "Björk", "IsFolder": true},
				{"Id": albumID, "Type": "MusicAlbum", "Name": "Homogenic", "IsFolder": true, "ParentId": music,
					"AlbumArtists": []map[string]string{{"Name": "Björk", "Id": artistID}}, "AlbumArtist": "Björk"},
				{"Id": trackID, "Type": "Audio", "Name": "Hunter", "ParentId": albumID, "AlbumId": albumID, "IndexNumber": 1, "MediaType": "Audio"},
			}},
		},
		"b": {
			views: []map[string]any{{"Id": movies, "Name": "Movies", "CollectionType": "movies"}},
			items: map[string][]map[string]any{movies: {
				{"Id": movieID, "Type": "Movie", "Name": "Alien", "ParentId": movies, "Overview": "In space", "ProductionYear": 1979,
					"ImageTags": map[string]string{"Primary": tagHex}, "ProviderIds": map[string]string{"Tmdb": "348"}, "MediaType": "Video"},
			}},
		},
	})
	e.login()

	views := e.do("GET", "/UserViews?userId="+e.userID, e.token, nil).json(t)["Items"].([]any)
	if len(views) != 2 {
		t.Fatalf("views = %v", views)
	}
	byName := map[string]map[string]any{}
	for _, v := range views {
		m := v.(map[string]any)
		byName[m["Name"].(string)] = m
	}
	musicView, movieView := byName["Music"], byName["Movies"]
	if musicView["CollectionType"] != "music" || movieView["CollectionType"] != "movies" {
		t.Fatalf("views = %v", views)
	}
	if r := e.do("GET", "/Users/"+e.userID+"/Views", e.token, nil).json(t); len(r["Items"].([]any)) != 2 {
		t.Error("legacy /Users/{id}/Views")
	}
	if r := e.do("GET", "/Items", e.token, nil).json(t); len(r["Items"].([]any)) != 2 {
		t.Error("/Items with no parent should list the libraries")
	}

	albums := e.do("GET", "/Users/"+e.userID+"/Items?ParentId="+musicView["Id"].(string)+"&Recursive=true&IncludeItemTypes=MusicAlbum", e.token, nil)
	al := albums.json(t)["Items"].([]any)
	if len(al) != 1 || al[0].(map[string]any)["Name"] != "Homogenic" || al[0].(map[string]any)["Id"] != e.uid("a", albumID) {
		t.Errorf("albums = %s", albums.body)
	}

	list := e.do("GET", "/Items?parentId="+movieView["Id"].(string)+"&recursive=true&includeItemTypes=Movie&fields=Overview", e.token, nil)
	mv := list.json(t)["Items"].([]any)
	if len(mv) != 1 || mv[0].(map[string]any)["Overview"] != "In space" {
		t.Errorf("movies = %s", list.body)
	}
	for _, leak := range []string{movieID, movies, albumID, music} {
		if bytes.Contains(list.body, []byte(leak)) || bytes.Contains(albums.body, []byte(leak)) {
			t.Errorf("upstream id %s leaked", leak)
		}
	}

	movie := e.uid("b", movieID)
	detail := e.do("GET", "/Users/"+e.userID+"/Items/"+movie, e.token, nil).json(t)
	if detail["ParentId"] != movieView["Id"] || detail["ProductionYear"] != float64(1979) {
		t.Errorf("detail = %v", detail)
	}

	// Artwork routes to the upstream that has it, for items and libraries.
	e.do("GET", "/Items/"+movie+"/Images/Primary?tag="+tagHex, "", nil)
	if req, _ := e.b.lastRequest(); req.URL.Path != "/Items/"+movieID+"/Images/Primary" || req.URL.Query().Get("tag") != tagHex {
		t.Errorf("movie image went to %s?%s", req.URL.Path, req.URL.RawQuery)
	}
	e.do("GET", "/Items/"+musicView["Id"].(string)+"/Images/Primary", "", nil)
	if req, _ := e.a.lastRequest(); req.URL.Path != "/Items/"+music+"/Images/Primary" {
		t.Errorf("library image went to %s", req.URL.Path)
	}

	artists := e.do("GET", "/Artists/AlbumArtists?parentId="+musicView["Id"].(string), e.token, nil).json(t)["Items"].([]any)
	if len(artists) != 1 || artists[0].(map[string]any)["Name"] != "Björk" {
		t.Errorf("album artists = %v", artists)
	}
	hints := e.do("GET", "/Search/Hints?searchTerm=hunt", e.token, nil).json(t)["SearchHints"].([]any)
	if len(hints) != 1 || hints[0].(map[string]any)["Name"] != "Hunter" {
		t.Errorf("hints = %v", hints)
	}
	latest := e.do("GET", "/Items/Latest?parentId="+musicView["Id"].(string), e.token, nil)
	if !strings.Contains(string(latest.body), "Homogenic") {
		t.Errorf("latest music (grouped by album) = %s", latest.body)
	}
	if r := e.do("GET", "/Items/"+hexID("dead"), e.token, nil); r.code != 404 {
		t.Errorf("unknown item = %d", r.code)
	}
	if r := e.do("GET", "/Users/"+hexID("99")+"/Items", e.token, nil); r.code != 403 {
		t.Errorf("other user's items = %d", r.code)
	}
}

func mediaSource(id, codec string, w, h, size int) map[string]any {
	return map[string]any{
		"Id": id, "Container": "mkv", "Size": size, "RunTimeTicks": 72_000_000_000, "Bitrate": size * 8,
		"SupportsDirectPlay": true, "SupportsDirectStream": true, "SupportsTranscoding": true, "Protocol": "File",
		"Name": "upstream file name", "MediaStreams": []map[string]any{
			{"Type": "Video", "Codec": codec, "Width": w, "Height": h}, {"Type": "Audio", "Codec": "aac", "Channels": 2}},
	}
}

func TestPlayback(t *testing.T) {
	e := newEnv(t, identity.Mode{})
	a4k := hexID("a4c")
	e.a.media = map[string][]map[string]any{shared["a"]: {
		mediaSource(shared["a"], "h264", 1920, 1080, 100), mediaSource(a4k, "hevc", 3840, 2160, 500)}}
	e.b.media = map[string][]map[string]any{
		shared["b"]: {mediaSource(shared["b"], "h264", 1920, 1080, 100)}, // identical to a's 1080p file
		bOnly:       {mediaSource(bOnly, "h264", 1280, 720, 50)},
	}
	e.login()
	item := e.uid("a", shared["a"])

	r := e.do("POST", "/Items/"+item+"/PlaybackInfo?userId="+e.userID, e.token, map[string]any{"UserId": e.userID, "DeviceProfile": map[string]any{}})
	if r.code != 200 {
		t.Fatalf("PlaybackInfo = %d %s", r.code, r.body)
	}
	info := r.json(t)
	psid := info["PlaySessionId"].(string)
	srcs := info["MediaSources"].([]any)
	if len(srcs) != 2 {
		t.Fatalf("want 2 versions (4K, 1080p shared by a and b), got %s", r.body)
	}
	v4k, v1080 := srcs[0].(map[string]any), srcs[1].(map[string]any)
	if v4k["Name"] != "4K HEVC" || v1080["Name"] != "1080p H264" {
		t.Errorf("version names = %v, %v", v4k["Name"], v1080["Name"])
	}
	if v4k["Id"] == item || v1080["Id"] == item || v4k["Id"] == v1080["Id"] {
		t.Errorf("version ids = %v %v", v4k["Id"], v1080["Id"])
	}
	for _, leak := range []string{shared["a"], shared["b"], a4k, hexID("9a"), hexID("9b"), "upstream file name",
		upstream.DeviceID("a", e.userID, "dev1"), e.a.userID, e.b.userID} {
		if bytes.Contains(r.body, []byte(leak)) {
			t.Errorf("PlaybackInfo leaks %q", leak)
		}
	}
	// Both upstreams were asked, as their own user, about their own copy.
	for _, f := range []*fakeUpstream{e.a, e.b} {
		reqs := f.find("/PlaybackInfo")
		if len(reqs) != 1 || reqs[0].path != "/Items/"+shared[f.name]+"/PlaybackInfo" || reqs[0].query.Get("userId") != f.userID ||
			!strings.Contains(reqs[0].body, f.userID) {
			t.Errorf("%s PlaybackInfo requests = %+v", f.name, reqs)
		}
	}

	// The transcoding URL names only JellyBloom IDs, the client's token and device.
	tu, _ := url.Parse(v1080["TranscodingUrl"].(string))
	tq := tu.Query()
	if tu.Path != "/videos/"+item+"/master.m3u8" || tq.Get("MediaSourceId") != v1080["Id"] || tq.Get("PlaySessionId") != psid ||
		tq.Get("ApiKey") != e.token || tq.Get("DeviceId") != "dev1" {
		t.Errorf("TranscodingUrl = %s", tu)
	}

	// Master playlist: routed to a (preferred source), with a's real IDs.
	pl := e.do("GET", tu.String(), "", nil)
	reqs := e.a.find("/master.m3u8")
	if pl.code != 200 || len(reqs) != 1 {
		t.Fatalf("master playlist = %d, a saw %d requests", pl.code, len(reqs))
	}
	got := reqs[0]
	if got.path != "/videos/"+shared["a"]+"/master.m3u8" || got.query.Get("MediaSourceId") != shared["a"] ||
		got.query.Get("PlaySessionId") != hexID("9a") || got.query.Get("DeviceId") != upstream.DeviceID("a", e.userID, "dev1") {
		t.Errorf("a got %s %v", got.path, got.query)
	}
	if !strings.Contains(got.auth, `DeviceId="`+upstream.DeviceID("a", e.userID, "dev1")+`"`) {
		t.Errorf("upstream auth device = %s", got.auth)
	}
	if k := got.query.Get("ApiKey"); k == "" || k == e.token {
		t.Errorf("upstream got ApiKey %q: want its own token, never the client's", k)
	}
	body := string(pl.body)
	if !strings.Contains(body, "PlaySessionId="+psid) || !strings.Contains(body, "ApiKey="+e.token) ||
		!strings.Contains(body, "DeviceId=dev1") || strings.Contains(body, hexID("9a")) || strings.Contains(body, shared["a"]) {
		t.Errorf("playlist not rewritten: %s", body)
	}

	// Segments relay from the same upstream.
	seg := e.do("GET", "/videos/"+item+"/hls1/main/0.mp4?MediaSourceId="+item+"&PlaySessionId="+psid+"&api_key="+e.token, "", nil)
	if string(seg.body) != "SEGMENT a" {
		t.Errorf("segment = %d %q", seg.code, seg.body)
	}

	// Direct play of the 4K version goes to a with a's 4K media source.
	e.do("GET", "/Videos/"+item+"/stream?static=true&MediaSourceId="+v4k["Id"].(string)+"&api_key="+e.token+"&DeviceId=dev1", "", nil)
	if reqs := e.a.find("/stream"); len(reqs) != 1 || reqs[0].query.Get("MediaSourceId") != a4k {
		t.Errorf("4K stream requests = %+v", reqs)
	}

	// Transcode cleanup and progress reports reach a with a's IDs.
	e.do("DELETE", "/Videos/ActiveEncodings?DeviceId=dev1&PlaySessionId="+psid, e.token, nil)
	if reqs := e.a.find("/ActiveEncodings"); len(reqs) != 1 || reqs[0].query.Get("PlaySessionId") != hexID("9a") ||
		reqs[0].query.Get("DeviceId") != upstream.DeviceID("a", e.userID, "dev1") {
		t.Errorf("ActiveEncodings = %+v", reqs)
	}
	e.do("POST", "/Sessions/Playing/Stopped", e.token, map[string]any{"ItemId": item, "MediaSourceId": v4k["Id"], "PlaySessionId": psid, "PositionTicks": 5})
	if reqs := e.a.find("/Sessions/Playing/Stopped"); len(reqs) != 1 || !strings.Contains(reqs[0].body, shared["a"]) ||
		!strings.Contains(reqs[0].body, a4k) || !strings.Contains(reqs[0].body, hexID("9a")) {
		t.Errorf("Stopped = %+v", reqs)
	}

	// Asking for one version keeps its ID and asks only its sources.
	r = e.do("POST", "/Items/"+item+"/PlaybackInfo", e.token, map[string]any{"MediaSourceId": v1080["Id"]})
	one := r.json(t)["MediaSources"].([]any)
	if len(one) != 1 || one[0].(map[string]any)["Id"] != v1080["Id"] {
		t.Errorf("narrowed = %s", r.body)
	}
	if reqs := e.b.find("/PlaybackInfo"); len(reqs) != 2 || reqs[1].query.Get("MediaSourceId") != shared["b"] {
		t.Errorf("b narrowed request = %+v", reqs)
	}

	// A single-version item keeps its item ID as media source ID.
	r = e.do("GET", "/Items/"+e.uid("b", bOnly)+"/PlaybackInfo", e.token, nil)
	if ms := r.json(t)["MediaSources"].([]any); len(ms) != 1 || ms[0].(map[string]any)["Id"] != e.uid("b", bOnly) ||
		ms[0].(map[string]any)["Name"] != "upstream file name" {
		t.Errorf("single version = %s", r.body)
	}

	// Another client device gets its own upstream device.
	e.do("POST", "/Items/"+item+"/PlaybackInfo", e.token, nil, "Authorization",
		`MediaBrowser Client="Other", Device="TV", DeviceId="dev2", Version="1", Token="`+e.token+`"`)
	reqs = e.a.find("/PlaybackInfo")
	if last := reqs[len(reqs)-1]; !strings.Contains(last.auth, `DeviceId="`+upstream.DeviceID("a", e.userID, "dev2")+`"`) {
		t.Errorf("second device auth = %s", last.auth)
	}

	// Hidden or unknown items have no playback info.
	if r := e.do("POST", "/Items/"+hexID("dead")+"/PlaybackInfo", e.token, nil); r.code != 404 {
		t.Errorf("unknown item PlaybackInfo = %d", r.code)
	}
}
