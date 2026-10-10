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
	"slices"
	"strconv"
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
	"github.com/NebulousOinkler/JellyBloom/internal/sources"
	"github.com/NebulousOinkler/JellyBloom/internal/upstream"
	"github.com/NebulousOinkler/JellyBloom/internal/userdata"
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
	members map[string][]string         // real collection ID -> member IDs (Jellyfin links them)
	files   map[string][]byte           // media source ID -> file served by static streams
	cut     int64                       // > 0: file bodies end after this many bytes
	failHLS int                         // != 0: HLS requests get this status
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
	case r.URL.Path == "/Users/Me":
		writeJSON(w, map[string]any{"Id": f.userID, "Name": "u", "Policy": map[string]any{"IsAdministrator": false}})
		return
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
		f.mu.Lock()
		status := f.failHLS
		f.mu.Unlock()
		if status != 0 {
			http.Error(w, "injected", status)
			return
		}
		w.Header().Set("Content-Type", "video/mp4")
		w.Write([]byte("SEGMENT " + f.name))
		return
	}
	if strings.Contains(r.URL.Path, "/stream") || strings.HasSuffix(r.URL.Path, "/Download") {
		f.mu.Lock()
		data, ok := f.files[queryFold(q, "MediaSourceId")]
		cut := f.cut
		f.mu.Unlock()
		if ok {
			serveFile(w, r, data, cut)
			return
		}
	}
	if strings.HasSuffix(r.URL.Path, "/stream") {
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("X-Got-Range", r.Header.Get("Range"))
		w.WriteHeader(http.StatusPartialContent)
		w.Write([]byte("BYTES " + aOnly + " " + tok))
		return
	}
	if ids, ok := f.members[q.Get("ParentId")]; ok && r.URL.Path == "/Items" {
		var items []map[string]any
		for _, id := range ids {
			items = append(items, map[string]any{"Id": id})
		}
		writeJSON(w, map[string]any{"Items": items, "TotalRecordCount": len(items)})
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

func queryFold(q url.Values, key string) string {
	for k, v := range q {
		if strings.EqualFold(k, key) && len(v) > 0 {
			return v[0]
		}
	}
	return ""
}

// serveFile answers a static stream like Jellyfin: Range support, and
// per-server Last-Modified. cut > 0 ends the body early.
func serveFile(w http.ResponseWriter, r *http.Request, data []byte, cut int64) {
	start, end := int64(0), int64(len(data))-1
	code := http.StatusOK
	if rng := r.Header.Get("Range"); rng != "" {
		a, b, _ := strings.Cut(strings.TrimPrefix(rng, "bytes="), "-")
		start, _ = strconv.ParseInt(a, 10, 64)
		if b != "" {
			end, _ = strconv.ParseInt(b, 10, 64)
		}
		end = min(end, int64(len(data))-1)
		code = http.StatusPartialContent
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
	}
	w.Header().Set("Content-Type", "video/x-matroska")
	w.Header().Set("Last-Modified", time.Now().UTC().Format(http.TimeFormat))
	w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
	w.WriteHeader(code)
	body := data[start : end+1]
	if cut > 0 && int64(len(body)) > cut {
		body = body[:cut]
	}
	w.Write(body)
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
	reg      *sources.Registry
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
			{"Id": aOnly, "Type": "Movie", "Name": "A", "ParentId": hexID("va"), "ProviderIds": map[string]string{"Tmdb": "2"}, "RunTimeTicks": 72_000_000_000, "MediaType": "Video"},
		}},
	},
	"b": {
		views: []map[string]any{{"Id": hexID("vb"), "Name": "Movies", "CollectionType": "movies"}},
		items: map[string][]map[string]any{hexID("vb"): {
			{"Id": shared["b"], "Type": "Movie", "Name": "Shared", "ParentId": hexID("vb"), "ProviderIds": map[string]string{"Tmdb": "1"}},
			{"Id": bOnly, "Type": "Movie", "Name": "B", "ParentId": hexID("vb"), "ProviderIds": map[string]string{"Tmdb": "3"}, "RunTimeTicks": 72_000_000_000, "MediaType": "Video"},
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
	if e.auth, err = auth.New(idx.DB(), []config.User{{Name: "me", PasswordHash: string(h)}, {Name: "boss", PasswordHash: string(h), Admin: true}}); err != nil {
		t.Fatal(err)
	}
	var clients []*upstream.Client
	for _, f := range []*fakeUpstream{e.a, e.b} {
		creds := upstream.Credentials{Password: func() (string, string, error) { return "u", "p", nil }}
		c, err := upstream.New(config.Upstream{Name: f.name, URL: f.srv.URL}, creds, upstream.Options{})
		if err != nil {
			t.Fatal(err)
		}
		clients = append(clients, c)
	}
	var cat *catalog.Catalog
	var px *proxy.Proxy
	e.reg, err = sources.New(idx.DB(), sources.Options{Upstreams: []string{"a", "b"}, StallTimeout: time.Second,
		Open: func(ctx context.Context, up, method, path string, q url.Values, hdr http.Header) (*http.Response, error) {
			return px.Open(ctx, up, proxy.Caller{}, method, path, q, hdr)
		},
		Library: sources.LibraryFunc(func(ctx context.Context, fn func(sources.LibraryItem) error) error {
			return e.res.EachFile(ctx, func(id, typ string, copies []identity.CopyDTO) error {
				it := sources.LibraryItem{ID: id, Type: typ}
				for _, c := range copies {
					it.Copies = append(it.Copies, sources.LibraryCopy{Upstream: c.Upstream, Item: c.ID, DTO: c.DTO})
				}
				return fn(it)
			})
		})})
	if err != nil {
		t.Fatal(err)
	}
	policy, _ := proxy.NewPolicy(nil)
	px, err = proxy.New(proxy.Options{Upstreams: clients, WebUpstream: "b", Resolver: e.res, ServerID: e.auth.ServerID(),
		Policy: policy, Hidden: func(id string) bool { return cat.Hidden(id) }, Sources: e.reg})
	if err != nil {
		t.Fatal(err)
	}
	ud, err := userdata.Open(idx.DB())
	if err != nil {
		t.Fatal(err)
	}
	cat = catalog.New(catalog.Options{DB: idx.DB(), Resolver: e.res, Upstreams: []string{"a", "b"},
		ServerID: e.auth.ServerID(), UpstreamIDs: px.UpstreamIDs, UserData: ud})
	e.res.OnRebuild(func(ctx context.Context) {
		if err := cat.Reload(ctx); err != nil {
			t.Error(err)
		}
	})
	if _, err := e.res.Rebuild(ctx); err != nil {
		t.Fatal(err)
	}
	e.server, err = New(Options{ServerName: "Bloom", DB: idx.DB(), Auth: e.auth, Proxy: px, Catalog: cat,
		Playback: playback.New(px, e.res, e.reg, nil), UserData: ud, Sources: e.reg, Resolver: e.res,
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

func (e *env) login() { e.loginAs("me") }

func (e *env) loginAs(name string) {
	e.t.Helper()
	r := e.do("POST", "/users/authenticatebyname", "", map[string]string{"Username": name, "Pw": "pw"})
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
		if ctx.Err() != nil {
			t.Fatal("socket never registered")
		}
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
	// (JellyBloom's own content checks also read the 1080p copies.)
	reqs = slices.DeleteFunc(e.a.find("/stream"), func(r recorded) bool { return !strings.Contains(r.auth, `Client="Test"`) })
	if len(reqs) != 1 || reqs[0].query.Get("MediaSourceId") != a4k {
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

func ud(t *testing.T, m map[string]any) map[string]any {
	t.Helper()
	if u, ok := m["UserData"].(map[string]any); ok {
		return u
	}
	return m
}

func TestUserState(t *testing.T) {
	e := newEnv(t, identity.Mode{})
	e.login()
	a := e.uid("a", aOnly)

	// Played and unplayed, modern and legacy routes.
	if r := e.do("POST", "/UserPlayedItems/"+a, e.token, nil); r.code != 200 || r.json(t)["Played"] != true {
		t.Fatalf("mark played = %d %s", r.code, r.body)
	}
	if d := e.do("GET", "/Users/"+e.userID+"/Items/"+a, e.token, nil).json(t); ud(t, d)["Played"] != true || ud(t, d)["PlayCount"] != float64(1) {
		t.Errorf("item user data = %v", ud(t, d))
	}
	e.do("DELETE", "/Users/"+e.userID+"/PlayedItems/"+a, e.token, nil)
	if d := e.do("GET", "/UserItems/"+a+"/UserData", e.token, nil).json(t); d["Played"] != false {
		t.Errorf("after unplayed = %v", d)
	}

	// Favorites and likes, and filtering by them.
	e.do("POST", "/Users/"+e.userID+"/FavoriteItems/"+a, e.token, nil)
	e.do("POST", "/UserItems/"+a+"/Rating?likes=true", e.token, nil)
	favs := e.do("GET", "/Items?Recursive=true&IsFavorite=true", e.token, nil).json(t)["Items"].([]any)
	if len(favs) != 1 || favs[0].(map[string]any)["Id"] != a || ud(t, favs[0].(map[string]any))["Likes"] != true {
		t.Errorf("favorites = %v", favs)
	}
	if r := e.do("POST", "/UserFavoriteItems/"+hexID("dead"), e.token, nil); r.code != 404 {
		t.Errorf("unknown item favorite = %d", r.code)
	}

	// Playback reports: recorded locally and forwarded upstream.
	body := func(pos int64) map[string]any { return map[string]any{"ItemId": a, "PositionTicks": pos} }
	e.do("POST", "/Sessions/Playing", e.token, body(0))
	e.do("POST", "/Sessions/Playing/Progress", e.token, body(36_000_000_000)) // 50% of 2h
	resume := e.do("GET", "/Users/"+e.userID+"/Items/Resume", e.token, nil).json(t)["Items"].([]any)
	if len(resume) != 1 || ud(t, resume[0].(map[string]any))["PlaybackPositionTicks"] != float64(36_000_000_000) {
		t.Errorf("resume = %v", resume)
	}
	e.do("POST", "/Sessions/Playing/Stopped", e.token, body(70_000_000_000)) // 97%: watched
	if d := e.do("GET", "/UserItems/"+a+"/UserData", e.token, nil).json(t); d["Played"] != true || d["PlaybackPositionTicks"] != float64(0) {
		t.Errorf("after stop at 97%% = %v", d)
	}
	if n := len(e.do("GET", "/Items/Resume", e.token, nil).json(t)["Items"].([]any)); n != 0 {
		t.Errorf("watched item still in resume (%d)", n)
	}
	for _, path := range []string{"/Sessions/Playing", "/Sessions/Playing/Progress", "/Sessions/Playing/Stopped"} {
		if reqs := e.a.find(path); len(reqs) != 1 || !strings.Contains(reqs[0].body, aOnly) {
			t.Errorf("%s not forwarded upstream with the real ID: %+v", path, reqs)
		}
	}

	// Legacy query-parameter reports.
	b := e.uid("b", bOnly)
	e.do("POST", "/Users/"+e.userID+"/PlayingItems/"+b+"?positionTicks=0", e.token, nil)
	e.do("DELETE", "/Users/"+e.userID+"/PlayingItems/"+b+"?positionTicks=20000000000", e.token, nil)
	if d := e.do("GET", "/UserItems/"+b+"/UserData", e.token, nil).json(t); d["PlaybackPositionTicks"] != float64(20_000_000_000) || d["Played"] != false {
		t.Errorf("legacy stop at 28%% = %v", d)
	}

	// Another user's data is not reachable.
	if r := e.do("POST", "/Users/"+hexID("99")+"/PlayedItems/"+a, e.token, nil); r.code != 403 {
		t.Errorf("other user = %d", r.code)
	}
}

func TestUserDataChangedPush(t *testing.T) {
	e := newEnv(t, identity.Mode{})
	e.login()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(e.srv.URL, "http")+"/socket?api_key="+e.token, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	var msg struct {
		MessageType string
		Data        struct {
			UserId       string
			UserDataList []map[string]any
		}
	}
	var first map[string]any // ForceKeepAlive; its Data is a number (a decode error would close the socket)
	if err := wsjson.Read(ctx, conn, &first); err != nil {
		t.Fatal(err)
	}
	for e.server.hub.count() == 0 {
		if ctx.Err() != nil {
			t.Fatal("socket never registered")
		}
		time.Sleep(5 * time.Millisecond)
	}
	a := e.uid("a", aOnly)
	e.do("POST", "/UserFavoriteItems/"+a, e.token, nil)
	if err := wsjson.Read(ctx, conn, &msg); err != nil || msg.MessageType != "UserDataChanged" ||
		msg.Data.UserId != e.userID || msg.Data.UserDataList[0]["ItemId"] != a || msg.Data.UserDataList[0]["IsFavorite"] != true {
		t.Errorf("push = %+v, %v", msg, err)
	}
}

func TestPlaylistsAndCollections(t *testing.T) {
	e := newEnv(t, identity.Mode{})
	e.login()
	a, b := e.uid("a", aOnly), e.uid("b", bOnly)

	r := e.do("POST", "/Playlists", e.token, map[string]any{"Name": "Mix", "Ids": []string{a, b, hexID("dead")}, "UserId": e.userID})
	pl := r.json(t)["Id"].(string)
	if r.code != 200 || pl == "" {
		t.Fatalf("create playlist = %d %s", r.code, r.body)
	}
	views := e.do("GET", "/UserViews", e.token, nil).json(t)["Items"].([]any)
	var plView string
	for _, v := range views {
		if v.(map[string]any)["CollectionType"] == "playlists" {
			plView = v.(map[string]any)["Id"].(string)
		}
	}
	if plView == "" {
		t.Fatalf("no Playlists library in %v", views)
	}
	lists := e.do("GET", "/Items?ParentId="+plView, e.token, nil).json(t)["Items"].([]any)
	if len(lists) != 1 || lists[0].(map[string]any)["Name"] != "Mix" || lists[0].(map[string]any)["ChildCount"] != float64(2) {
		t.Errorf("playlists = %v", lists)
	}
	if typed := e.do("GET", "/Items?IncludeItemTypes=Playlist&Recursive=true", e.token, nil).json(t)["Items"].([]any); len(typed) != 1 {
		t.Errorf("playlists by type = %v", typed)
	}

	entries := e.do("GET", "/Playlists/"+pl+"/Items", e.token, nil).json(t)["Items"].([]any)
	if len(entries) != 2 || entries[0].(map[string]any)["Id"] != a || entries[1].(map[string]any)["PlaylistItemId"] == nil {
		t.Fatalf("entries = %v (unknown item must be dropped)", entries)
	}
	second := entries[1].(map[string]any)["PlaylistItemId"].(string)
	e.do("POST", "/Playlists/"+pl+"/Items/"+second+"/Move/0", e.token, nil)
	e.do("POST", "/Playlists/"+pl+"/Items?ids="+a, e.token, nil)
	ids := func() []string {
		var out []string
		for _, it := range e.do("GET", "/Items?ParentId="+pl, e.token, nil).json(t)["Items"].([]any) {
			out = append(out, it.(map[string]any)["Id"].(string))
		}
		return out
	}
	if got := ids(); !slices.Equal(got, []string{b, a, a}) {
		t.Errorf("after move+add = %v", got)
	}
	e.do("DELETE", "/Playlists/"+pl+"/Items?entryIds="+second, e.token, nil)
	if got := ids(); !slices.Equal(got, []string{a, a}) {
		t.Errorf("after remove = %v", got)
	}
	if d := e.do("GET", "/Items/"+pl, e.token, nil).json(t); d["Type"] != "Playlist" || d["MediaType"] != "Video" {
		t.Errorf("playlist detail = %v", d)
	}

	// Collections are shared and live in a Collections library.
	r = e.do("POST", "/Collections?name=Duo&ids="+a+","+b, e.token, nil)
	col := r.json(t)["Id"].(string)
	if items := e.do("GET", "/Items?ParentId="+col, e.token, nil).json(t)["Items"].([]any); len(items) != 2 {
		t.Errorf("collection items = %v", items)
	}
	e.do("DELETE", "/Collections/"+col+"/Items?ids="+b, e.token, nil)
	if items := e.do("GET", "/Items?ParentId="+col, e.token, nil).json(t)["Items"].([]any); len(items) != 1 {
		t.Errorf("after remove = %v", items)
	}
	boxsets := e.do("GET", "/Items?ParentId="+identity.DeriveID("view:boxsets:collections"), e.token, nil).json(t)["Items"].([]any)
	if len(boxsets) != 1 || boxsets[0].(map[string]any)["Type"] != "BoxSet" {
		t.Errorf("collections library = %v", boxsets)
	}

	// Deleting: own lists yes; upstream media never.
	if r := e.do("DELETE", "/Items/"+pl, e.token, nil); r.code != 204 {
		t.Errorf("delete playlist = %d", r.code)
	}
	if r := e.do("DELETE", "/Items/"+col, e.token, nil); r.code != 204 {
		t.Errorf("delete collection = %d", r.code)
	}
	if r := e.do("DELETE", "/Items/"+a, e.token, nil); r.code != 403 {
		t.Errorf("delete upstream item = %d, want 403", r.code)
	}
	if r := e.do("GET", "/Playlists/"+pl, e.token, nil); r.code != 404 {
		t.Errorf("deleted playlist = %d", r.code)
	}
}

func dashed(id string) string {
	return id[:8] + "-" + id[8:12] + "-" + id[12:16] + "-" + id[16:20] + "-" + id[20:]
}

func TestPlaylistSortingAndDashedIDs(t *testing.T) {
	e := newEnv(t, identity.Mode{})
	e.login()
	a, b, sh := e.uid("a", aOnly), e.uid("b", bOnly), e.uid("a", shared["a"])
	pl := e.do("POST", "/Playlists", e.token, map[string]any{"Name": "Mix", "Ids": []string{b, sh, a}}).json(t)["Id"].(string)
	names := func(path string) []string {
		var out []string
		for _, it := range e.do("GET", path, e.token, nil).json(t)["Items"].([]any) {
			out = append(out, it.(map[string]any)["Name"].(string))
		}
		return out
	}
	if got := names("/Items?ParentId=" + pl); !slices.Equal(got, []string{"B", "Shared", "A"}) {
		t.Errorf("list order = %v", got)
	}
	if got := names("/Items?ParentId=" + pl + "&SortBy=SortName"); !slices.Equal(got, []string{"A", "B", "Shared"}) {
		t.Errorf("sorted by name = %v", got)
	}
	seen := map[string]bool{}
	for range 30 {
		seen[strings.Join(names("/Items?ParentId="+pl+"&SortBy=Random"), ",")] = true
	}
	if len(seen) < 2 {
		t.Errorf("SortBy=Random (shuffle) never changed the order: %v", seen)
	}

	// Jellyfin 12.2 clients send dashed GUIDs in paths.
	if r := e.do("POST", "/Playlists/"+dashed(pl)+"/Items?ids="+dashed(a), e.token, nil); r.code != 204 {
		t.Errorf("add via dashed IDs = %d", r.code)
	}
	col := e.do("POST", "/Collections?name=Duo&ids="+a, e.token, nil).json(t)["Id"].(string)
	if r := e.do("POST", "/Collections/"+dashed(col)+"/Items?ids="+b, e.token, nil); r.code != 204 {
		t.Errorf("add to collection via dashed ID = %d", r.code)
	}
	if got := names("/Items?ParentId=" + dashed(col)); len(got) != 2 {
		t.Errorf("collection via dashed ID = %v", got)
	}

	// Playlists and collections can be favorited and marked played, as in Jellyfin.
	if r := e.do("POST", "/UserFavoriteItems/"+pl, e.token, nil); r.code != 200 {
		t.Errorf("favorite playlist = %d", r.code)
	}
	if d := e.do("GET", "/Items/"+pl, e.token, nil).json(t); ud(t, d)["IsFavorite"] != true {
		t.Errorf("playlist user data = %v", d["UserData"])
	}
	if r := e.do("POST", "/UserPlayedItems/"+col, e.token, nil); r.code != 200 {
		t.Errorf("mark collection played = %d", r.code)
	}
	if d := e.do("GET", "/Items/"+b, e.token, nil).json(t); ud(t, d)["Played"] != true {
		t.Errorf("collection member not marked played: %v", d["UserData"])
	}
}

func TestUpstreamCollections(t *testing.T) {
	saga := hexID("c5")
	data := map[string]library{"a": {
		views: []map[string]any{{"Id": hexID("va"), "Name": "Movies", "CollectionType": "movies"},
			{"Id": hexID("vc"), "Name": "Collections", "CollectionType": "boxsets"}},
		items: map[string][]map[string]any{
			hexID("va"): defaultData["a"].items[hexID("va")],
			// Jellyfin lists only the BoxSet under its library; members are linked.
			hexID("vc"): {{"Id": saga, "Type": "BoxSet", "Name": "Saga", "IsFolder": true, "ParentId": hexID("vc"), "ChildCount": 2}},
		},
	}, "b": defaultData["b"]}
	e := newEnvWith(t, identity.Mode{}, data)
	e.a.members = map[string][]string{saga: {aOnly, shared["a"]}}
	e.login()
	sagaID, a, b, sh := e.uid("a", saga), e.uid("a", aOnly), e.uid("b", bOnly), e.uid("a", shared["a"])
	members := func() []string {
		var out []string
		for _, it := range e.do("GET", "/Items?ParentId="+sagaID+"&SortBy=SortName", e.token, nil).json(t)["Items"].([]any) {
			out = append(out, it.(map[string]any)["Id"].(string))
		}
		return out
	}
	if got := members(); !slices.Equal(got, []string{a, sh}) {
		t.Fatalf("upstream collection members = %v, want %v", got, []string{a, sh})
	}
	// Edits are kept in JellyBloom; the upstream is not asked to change.
	if r := e.do("POST", "/Collections/"+sagaID+"/Items?ids="+b, e.token, nil); r.code != 204 {
		t.Errorf("add to upstream collection = %d", r.code)
	}
	if r := e.do("DELETE", "/Collections/"+sagaID+"/Items?ids="+sh, e.token, nil); r.code != 204 {
		t.Errorf("remove from upstream collection = %d", r.code)
	}
	if got := members(); !slices.Equal(got, []string{a, b}) {
		t.Errorf("after edits = %v, want %v", got, []string{a, b})
	}
	for _, rq := range e.a.history {
		if rq.method != "GET" {
			t.Errorf("upstream got a %s %s: collections must not be changed upstream", rq.method, rq.path)
		}
	}
	// Marking a collection played marks its members.
	if r := e.do("POST", "/UserPlayedItems/"+sagaID, e.token, nil); r.code != 200 {
		t.Errorf("mark upstream collection played = %d", r.code)
	}
	for _, id := range []string{a, b} {
		if d := e.do("GET", "/Items/"+id, e.token, nil).json(t); ud(t, d)["Played"] != true {
			t.Errorf("member %s not marked played: %v", id, ud(t, d))
		}
	}
	if r := e.do("DELETE", "/Items/"+sagaID, e.token, nil); r.code != 403 {
		t.Errorf("deleting an upstream collection = %d, want 403", r.code)
	}
	if r := e.do("POST", "/Collections/"+a+"/Items?ids="+b, e.token, nil); r.code != 404 {
		t.Errorf("adding to a movie = %d, want 404", r.code)
	}
}

// fileData is a library whose shared movie has a media file on both
// upstreams, as indexed with Fields=MediaSources.
func fileData(sizeA, sizeB int) map[string]library {
	ms := func(id string, size int) []map[string]any {
		return []map[string]any{mediaSource(id, "h264", 1920, 1080, size)}
	}
	data := map[string]library{}
	for up, lib := range defaultData {
		items := map[string][]map[string]any{}
		for v, list := range lib.items {
			for _, it := range list {
				c := map[string]any{}
				for k, val := range it {
					c[k] = val
				}
				if c["Id"] == shared[up] {
					c["MediaSources"] = ms(shared[up], map[string]int{"a": sizeA, "b": sizeB}[up])
				}
				items[v] = append(items[v], c)
			}
		}
		data[up] = library{views: lib.views, items: items}
	}
	return data
}

// withFiles stores file contents on the fakes and their PlaybackInfo.
func (e *env) withFiles(a, b []byte) {
	e.a.media = map[string][]map[string]any{shared["a"]: {mediaSource(shared["a"], "h264", 1920, 1080, len(a))}}
	e.b.media = map[string][]map[string]any{shared["b"]: {mediaSource(shared["b"], "h264", 1920, 1080, len(b))}}
	e.a.files = map[string][]byte{shared["a"]: a}
	e.b.files = map[string][]byte{shared["b"]: b}
}

func testFile(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*7 + i/251)
	}
	return b
}

func TestDirectPlayFailsOverMidStream(t *testing.T) {
	data := testFile(3 << 20)
	e := newEnvWith(t, identity.Mode{}, fileData(len(data), len(data)))
	e.withFiles(data, data)
	e.login()
	item := e.uid("a", shared["a"])
	info := e.do("POST", "/Items/"+item+"/PlaybackInfo", e.token, map[string]any{"DeviceProfile": map[string]any{}}).json(t)
	srcs := info["MediaSources"].([]any)
	if len(srcs) != 1 {
		t.Fatalf("identical copies must be one version: %v", srcs)
	}
	psid := info["PlaySessionId"].(string)
	msID := srcs[0].(map[string]any)["Id"].(string)

	// a breaks off after 1 MiB; the client still gets the whole file.
	e.a.mu.Lock()
	e.a.cut = 1 << 20
	e.a.mu.Unlock()
	url := "/Videos/" + item + "/stream.mkv?Static=true&MediaSourceId=" + msID + "&PlaySessionId=" + psid + "&api_key=" + e.token
	r := e.do("GET", url, "", nil)
	if r.code != 200 || !bytes.Equal(r.body, data) {
		t.Fatalf("stream = %d, %d bytes, identical %v", r.code, len(r.body), bytes.Equal(r.body, data))
	}
	if r.header.Get("Last-Modified") != "" {
		t.Error("an upstream's Last-Modified reached the client")
	}
	// The client's range requests go on to b, and so do its reports.
	r = e.do("GET", url, "", nil, "Range", "bytes=100-199")
	if r.code != http.StatusPartialContent || !bytes.Equal(r.body, data[100:200]) {
		t.Errorf("range = %d %q", r.code, r.header.Get("Content-Range"))
	}
	e.do("POST", "/Sessions/Playing/Progress", e.token, map[string]any{"ItemId": item, "MediaSourceId": msID, "PlaySessionId": psid, "PositionTicks": 1})
	if len(e.b.find("/Sessions/Playing/Progress")) != 1 || len(e.a.find("/Sessions/Playing/Progress")) != 0 {
		t.Errorf("progress went to a %d, b %d times", len(e.a.find("/Sessions/Playing/Progress")), len(e.b.find("/Sessions/Playing/Progress")))
	}
}

func TestPlaybackLeavesOutTruncatedCopy(t *testing.T) {
	data := testFile(2 << 20)
	short := data[:len(data)*6/10]
	e := newEnvWith(t, identity.Mode{}, fileData(len(data), len(short)))
	e.withFiles(data, short)
	e.login()
	item := e.uid("a", shared["a"])
	info := e.do("POST", "/Items/"+item+"/PlaybackInfo", e.token, map[string]any{}).json(t)
	if srcs := info["MediaSources"].([]any); len(srcs) != 1 || srcs[0].(map[string]any)["Size"] != float64(len(data)) {
		t.Fatalf("want only a's complete copy: %v", srcs)
	}
	// Without a, b's truncated copy is all there is: it is offered.
	e.a.srv.Close()
	info = e.do("POST", "/Items/"+item+"/PlaybackInfo", e.token, map[string]any{}).json(t)
	if srcs := info["MediaSources"].([]any); len(srcs) != 1 || srcs[0].(map[string]any)["Size"] != float64(len(short)) {
		t.Errorf("only copy: %v", srcs)
	}

	// The report names it, for admins only.
	if r := e.do("GET", "/JellyBloom/Report", e.token, nil); r.code != http.StatusForbidden {
		t.Errorf("non-admin report = %d", r.code)
	}
	e.loginAs("boss")
	rep := e.do("GET", "/JellyBloom/Report", e.token, nil).json(t)
	broken, _ := rep["Broken"].([]any)
	if len(broken) != 1 || !strings.Contains(fmt.Sprint(broken[0]), "truncated") {
		t.Errorf("report broken = %v", rep["Broken"])
	}
	for _, u := range rep["Upstreams"].([]any) {
		if m := u.(map[string]any); m["Name"] == "a" && m["Reachable"] != false {
			t.Errorf("a is down but reported %v", m)
		}
	}
}

func TestHLSFailsOverToAnotherUpstream(t *testing.T) {
	e := newEnv(t, identity.Mode{})
	e.a.media = map[string][]map[string]any{shared["a"]: {mediaSource(shared["a"], "h264", 1920, 1080, 100)}}
	e.b.media = map[string][]map[string]any{shared["b"]: {mediaSource(shared["b"], "h264", 1920, 1080, 100)}}
	e.login()
	item := e.uid("a", shared["a"])
	info := e.do("POST", "/Items/"+item+"/PlaybackInfo", e.token, map[string]any{}).json(t)
	psid := info["PlaySessionId"].(string)
	msID := info["MediaSources"].([]any)[0].(map[string]any)["Id"].(string)
	// Segment URLs carry a's media source Tag, a GUID only a knows.
	tag := e.uid("a", hexID("7ac"))
	seg := func(n int) resp {
		return e.do("GET", fmt.Sprintf("/videos/%s/hls1/main/%d.ts?MediaSourceId=%s&PlaySessionId=%s&Tag=%s&api_key=%s", item, n, msID, psid, tag, e.token), "", nil)
	}
	if r := seg(0); string(r.body) != "SEGMENT a" {
		t.Fatalf("segment 0 = %q", r.body)
	}
	e.a.mu.Lock()
	e.a.failHLS = http.StatusInternalServerError
	e.a.mu.Unlock()
	if r := seg(1); r.code != 200 || string(r.body) != "SEGMENT b" {
		t.Fatalf("segment 1 = %d %q", r.code, r.body)
	}
	// b got b's IDs, including b's own play session.
	got := e.b.find(".ts")
	if len(got) != 1 || got[0].path != "/videos/"+shared["b"]+"/hls1/main/1.ts" || got[0].query.Get("PlaySessionId") != hexID("9b") {
		t.Errorf("b got %+v", got)
	}
	// The session now lives on b: the next segment does not try a first.
	before := len(e.a.find(".ts"))
	seg(2)
	if len(e.a.find(".ts")) != before {
		t.Error("segment 2 tried a again")
	}
}
