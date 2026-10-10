package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
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
	"github.com/NebulousOinkler/JellyBloom/internal/config"
	"github.com/NebulousOinkler/JellyBloom/internal/identity"
	"github.com/NebulousOinkler/JellyBloom/internal/index"
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
	last    *http.Request
	lastRaw []byte
}

func newFakeUpstream(t *testing.T, name string) *fakeUpstream {
	f := &fakeUpstream{name: name, userID: hexID("f" + name), server: hexID("5" + name)}
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
		f.mu.Unlock()
	}

	switch {
	case r.URL.Path == "/Users/AuthenticateByName":
		f.mu.Lock()
		f.logins++
		f.token = hexID(fmt.Sprintf("%s%d", "c"+f.name, f.logins))
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

	f.mu.Lock()
	tok := f.token
	f.mu.Unlock()
	m := tokenRe.FindStringSubmatch(r.Header.Get("Authorization"))
	if m == nil || m[1] != tok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if strings.HasSuffix(r.URL.Path, "/stream") {
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("X-Got-Range", r.Header.Get("Range"))
		w.WriteHeader(http.StatusPartialContent)
		w.Write([]byte("BYTES " + aOnly + " " + tok))
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
	f.token = "revoked"
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

func newEnv(t *testing.T, mode identity.Mode) *env {
	t.Helper()
	ctx := context.Background()
	idx, err := index.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { idx.Close() })

	e := &env{t: t, a: newFakeUpstream(t, "a"), b: newFakeUpstream(t, "b")}
	movies := map[string][]map[string]any{
		"a": {{"Id": shared["a"], "Type": "Movie", "Name": "Shared", "ProviderIds": map[string]string{"Tmdb": "1"}},
			{"Id": aOnly, "Type": "Movie", "Name": "A", "ProviderIds": map[string]string{"Tmdb": "2"}}},
		"b": {{"Id": shared["b"], "Type": "Movie", "Name": "Shared", "ProviderIds": map[string]string{"Tmdb": "1"}},
			{"Id": bOnly, "Type": "Movie", "Name": "B", "ProviderIds": map[string]string{"Tmdb": "3"}}},
	}
	for up, items := range movies {
		fs, _ := idx.BeginFullSync(ctx, up)
		var raws []json.RawMessage
		for _, it := range items {
			b, _ := json.Marshal(it)
			raws = append(raws, b)
		}
		if err := fs.PutItems(ctx, "v", raws); err != nil {
			t.Fatal(err)
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
	if _, err := e.res.Rebuild(ctx); err != nil {
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
	policy, _ := proxy.NewPolicy(nil)
	px, err := proxy.New(proxy.Options{Upstreams: clients, WebUpstream: "b", Resolver: e.res, ServerID: e.auth.ServerID(), Policy: policy})
	if err != nil {
		t.Fatal(err)
	}
	e.server, err = New(Options{ServerName: "Bloom", DB: idx.DB(), Auth: e.auth, Proxy: px,
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
	if auth := upReq.Header.Get("Authorization"); strings.Contains(auth, e.token) || !strings.Contains(auth, `Client="JellyBloom"`) {
		t.Errorf("upstream got client credentials: %q", auth)
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
