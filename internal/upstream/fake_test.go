package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/NebulousOinkler/JellyBloom/internal/config"
)

// fakeJellyfin implements the slice of the Jellyfin API the upstream client uses.
type fakeJellyfin struct {
	t   *testing.T
	srv *httptest.Server

	mu         sync.Mutex
	users      map[string]string // name -> id
	admins     map[string]bool   // user id -> administrator
	devices    map[string]string // token -> device ID it was issued to
	qcEnabled  bool
	qc         map[string]*fakeQC // secret -> pending Quick Connect request
	qcSeq      int
	password   string
	apiKey     string
	tokens     map[string]string // token -> user id
	logins     int
	items      []map[string]any
	failNext   int // respond 503 to this many authenticated requests
	lastQuery  map[string]string
	lastAuth   string
	keepAlives int
	keepAliveS float64 // ForceKeepAlive interval sent on connect
	sockets    chan *fakeSocket
}

type fakeSocket struct {
	conn *websocket.Conn
	done chan struct{}
}

var tokenRe = regexp.MustCompile(`Token="([^"]*)"`)

func newFake(t *testing.T) *fakeJellyfin {
	f := &fakeJellyfin{
		t:          t,
		users:      map[string]string{"me": "user-me", "guest": "user-guest", "boss": "user-boss"},
		admins:     map[string]bool{"user-boss": true},
		devices:    map[string]string{},
		qcEnabled:  true,
		qc:         map[string]*fakeQC{},
		password:   "pw",
		apiKey:     "admin-key",
		tokens:     map[string]string{},
		keepAliveS: 0.2,
		sockets:    make(chan *fakeSocket, 4),
	}
	for i := range 25 {
		f.items = append(f.items, map[string]any{"Id": fmt.Sprintf("item%02d", i), "Name": fmt.Sprintf("Item %d", i), "Type": "Movie"})
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /System/Info/Public", f.publicInfo)
	mux.HandleFunc("POST /Users/AuthenticateByName", f.authenticate)
	mux.HandleFunc("GET /Users", f.auth(f.listUsers))
	mux.HandleFunc("GET /UserViews", f.auth(f.views))
	mux.HandleFunc("GET /Items", f.auth(f.itemsHandler))
	mux.HandleFunc("GET /Artists", f.auth(f.itemsHandler))
	mux.HandleFunc("GET /socket", f.socket)
	mux.HandleFunc("GET /Users/Me", f.auth(f.me))
	mux.HandleFunc("POST /Sessions/Logout", f.auth(f.logout))
	mux.HandleFunc("GET /QuickConnect/Enabled", f.qcEnabledHandler)
	mux.HandleFunc("POST /QuickConnect/Initiate", f.qcInitiate)
	mux.HandleFunc("GET /QuickConnect/Connect", f.qcConnect)
	mux.HandleFunc("POST /QuickConnect/Authorize", f.auth(f.qcAuthorize))
	mux.HandleFunc("POST /Users/AuthenticateWithQuickConnect", f.qcAuthenticate)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeJellyfin) client(t *testing.T, creds Credentials) *Client {
	t.Helper()
	u := config.Upstream{Name: "fake", URL: f.srv.URL}
	c, err := New(u, creds, Options{MaxRetries: 3, BaseBackoff: time.Millisecond, MaxBackoff: 5 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func (f *fakeJellyfin) tokenFrom(r *http.Request) string {
	if m := tokenRe.FindStringSubmatch(r.Header.Get("Authorization")); m != nil {
		return m[1]
	}
	return r.URL.Query().Get("api_key")
}

// userFor returns the user for a token; the API key acts as admin ("").
func (f *fakeJellyfin) userFor(token string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if token == f.apiKey {
		return "", true
	}
	u, ok := f.tokens[token]
	return u, ok
}

func (f *fakeJellyfin) auth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := f.userFor(f.tokenFrom(r)); !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		f.mu.Lock()
		f.lastAuth = r.Header.Get("Authorization")
		fail := f.failNext > 0
		if fail {
			f.failNext--
		}
		f.mu.Unlock()
		if fail {
			w.Header().Set("Retry-After", "0")
			http.Error(w, "busy", http.StatusServiceUnavailable)
			return
		}
		h(w, r)
	}
}

func (f *fakeJellyfin) revokeAll() {
	f.mu.Lock()
	f.tokens = map[string]string{}
	f.mu.Unlock()
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func (f *fakeJellyfin) publicInfo(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"ServerName": "Fake", "Version": "10.10.7", "Id": "server-1", "ProductName": "Jellyfin Server"})
}

func (f *fakeJellyfin) authenticate(w http.ResponseWriter, r *http.Request) {
	var body struct{ Username, Pw string }
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	id, ok := f.users[body.Username]
	if !ok || body.Pw != f.password {
		http.Error(w, "bad credentials", http.StatusUnauthorized)
		return
	}
	f.logins++
	token := fmt.Sprintf("token-%d", f.logins)
	f.tokens[token] = id
	f.devices[token] = deviceOf(r)
	writeJSON(w, map[string]any{"AccessToken": token, "User": map[string]any{"Id": id, "Name": body.Username}})
}

func (f *fakeJellyfin) listUsers(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []map[string]string
	for name, id := range f.users {
		out = append(out, map[string]string{"Name": name, "Id": id})
	}
	writeJSON(w, out)
}

func (f *fakeJellyfin) recordQuery(r *http.Request) {
	q := map[string]string{}
	for k := range r.URL.Query() {
		q[k] = r.URL.Query().Get(k)
	}
	f.mu.Lock()
	f.lastQuery = q
	f.mu.Unlock()
}

func (f *fakeJellyfin) views(w http.ResponseWriter, r *http.Request) {
	f.recordQuery(r)
	writeJSON(w, map[string]any{
		"Items":            []map[string]any{{"Id": "lib-music", "Name": "Music", "CollectionType": "music"}},
		"TotalRecordCount": 1,
	})
}

func (f *fakeJellyfin) itemsHandler(w http.ResponseWriter, r *http.Request) {
	f.recordQuery(r)
	q := r.URL.Query()
	start, _ := strconv.Atoi(q.Get("startIndex"))
	limit, _ := strconv.Atoi(q.Get("limit"))
	f.mu.Lock()
	all := f.items
	f.mu.Unlock()
	end := len(all)
	if limit > 0 && start+limit < end {
		end = start + limit
	}
	page := []map[string]any{}
	if start < len(all) {
		page = all[start:end]
	}
	writeJSON(w, map[string]any{"Items": page, "TotalRecordCount": len(all), "StartIndex": start})
}

func (f *fakeJellyfin) socket(w http.ResponseWriter, r *http.Request) {
	if _, ok := f.userFor(f.tokenFrom(r)); !ok {
		http.Error(w, "forbidden", http.StatusForbidden) // as Jellyfin does for a revoked token
		return
	}
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	s := &fakeSocket{conn: conn, done: make(chan struct{})}
	defer close(s.done)
	ctx := r.Context()
	f.mu.Lock()
	interval := f.keepAliveS
	f.mu.Unlock()
	if err := wsjson.Write(ctx, conn, map[string]any{"MessageType": "ForceKeepAlive", "Data": interval}); err != nil {
		return
	}
	f.sockets <- s
	for {
		var msg struct{ MessageType string }
		if err := wsjson.Read(ctx, conn, &msg); err != nil {
			return
		}
		if msg.MessageType == "KeepAlive" {
			f.mu.Lock()
			f.keepAlives++
			f.mu.Unlock()
		}
	}
}

func (f *fakeJellyfin) nextSocket(t *testing.T) *fakeSocket {
	t.Helper()
	select {
	case s := <-f.sockets:
		return s
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for socket connection")
		return nil
	}
}

func (f *fakeJellyfin) query(key string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastQuery[key]
}

func (f *fakeJellyfin) get(fn func() int) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return fn()
}

func testCtx(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

var deviceRe = regexp.MustCompile(`DeviceId="([^"]*)"`)

func deviceOf(r *http.Request) string {
	if m := deviceRe.FindStringSubmatch(r.Header.Get("Authorization")); m != nil {
		return m[1]
	}
	return ""
}

type fakeQC struct {
	code, device, user string // user: approving user ID, "" while pending
}

func pwCreds(user, pw string) Credentials {
	return Credentials{Password: func() (string, string, error) { return user, pw, nil }}
}

func apiCreds(key, user string) Credentials {
	return Credentials{AllowAdmin: true, Load: func(context.Context) (Secret, error) {
		return Secret{Kind: KindAPIKey, Value: key, User: user}, nil
	}}
}

// storeCreds keeps the secret in *s, like a secret store.
func storeCreds(s *Secret) Credentials {
	return Credentials{
		Load: func(context.Context) (Secret, error) {
			if s.Value == "" {
				return Secret{}, ErrNotPaired
			}
			return *s, nil
		},
		Save: func(_ context.Context, v Secret) error { *s = v; return nil },
	}
}

// issue mints a token for user as device (test setup).
func (f *fakeJellyfin) issue(user, device string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.logins++
	tok := fmt.Sprintf("token-%d", f.logins)
	f.tokens[tok] = f.users[user]
	f.devices[tok] = device
	return tok
}

func (f *fakeJellyfin) valid(tok string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.tokens[tok]
	return ok
}

func (f *fakeJellyfin) me(w http.ResponseWriter, r *http.Request) {
	uid, _ := f.userFor(f.tokenFrom(r))
	f.mu.Lock()
	defer f.mu.Unlock()
	for name, id := range f.users {
		if id == uid {
			writeJSON(w, map[string]any{"Id": id, "Name": name, "Policy": map[string]any{"IsAdministrator": f.admins[id]}})
			return
		}
	}
	http.Error(w, "no user", http.StatusBadRequest)
}

func (f *fakeJellyfin) logout(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	delete(f.tokens, f.tokenFrom(r))
	f.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (f *fakeJellyfin) qcEnabledHandler(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	on := f.qcEnabled
	f.mu.Unlock()
	writeJSON(w, on)
}

func (f *fakeJellyfin) qcInitiate(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.qcEnabled {
		http.Error(w, "quick connect disabled", http.StatusUnauthorized)
		return
	}
	f.qcSeq++
	q := &fakeQC{code: fmt.Sprintf("%06d", f.qcSeq), device: deviceOf(r)}
	secret := fmt.Sprintf("secret-%d", f.qcSeq)
	f.qc[secret] = q
	writeJSON(w, map[string]any{"Code": q.code, "Secret": secret, "Authenticated": false})
}

func (f *fakeJellyfin) qcConnect(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	q := f.qc[r.URL.Query().Get("secret")]
	if q == nil {
		http.Error(w, "unknown secret", http.StatusNotFound)
		return
	}
	writeJSON(w, map[string]any{"Authenticated": q.user != ""})
}

// approve approves a pending code as user (what a person does in their app).
func (f *fakeJellyfin) approve(code, user string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, q := range f.qc {
		if q.code == code {
			q.user = f.users[user]
			return true
		}
	}
	return false
}

func (f *fakeJellyfin) qcAuthorize(w http.ResponseWriter, r *http.Request) {
	uid, _ := f.userFor(f.tokenFrom(r))
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, q := range f.qc {
		if q.code == r.URL.Query().Get("code") {
			q.user = uid
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}
	http.Error(w, "unknown code", http.StatusNotFound)
}

func (f *fakeJellyfin) qcAuthenticate(w http.ResponseWriter, r *http.Request) {
	var body struct{ Secret string }
	json.NewDecoder(r.Body).Decode(&body)
	f.mu.Lock()
	defer f.mu.Unlock()
	q := f.qc[body.Secret]
	if q == nil || q.user == "" {
		http.Error(w, "not approved", http.StatusUnauthorized)
		return
	}
	delete(f.qc, body.Secret)
	f.logins++
	tok := fmt.Sprintf("qc-token-%d", f.logins)
	f.tokens[tok] = q.user
	f.devices[tok] = deviceOf(r)
	writeJSON(w, map[string]any{"AccessToken": tok, "User": map[string]any{"Id": q.user}})
}
