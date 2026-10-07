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
		users:      map[string]string{"me": "user-me", "guest": "user-guest"},
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
	mux.HandleFunc("GET /socket", f.socket)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeJellyfin) client(t *testing.T, u config.Upstream) *Client {
	t.Helper()
	u.Name = "fake"
	u.URL = f.srv.URL
	c, err := New(u, Options{MaxRetries: 3, BaseBackoff: time.Millisecond, MaxBackoff: 5 * time.Millisecond})
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
		http.Error(w, "unauthorized", http.StatusUnauthorized)
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
