package pairing

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NebulousOinkler/JellyBloom/internal/config"
	"github.com/NebulousOinkler/JellyBloom/internal/secrets"
	"github.com/NebulousOinkler/JellyBloom/internal/upstream"
)

// fake is a Jellyfin server with Quick Connect, passwords, and an API key.
type fake struct {
	srv       *httptest.Server
	mu        sync.Mutex
	qcEnabled bool
	approveAs string            // user that approves Quick Connect codes ("" = never)
	tokens    map[string]string // token -> user
	pending   map[string]string // secret -> approving user ("" while pending)
	n         int
}

var users = map[string]bool{"bloom": false, "boss": true} // name -> admin

func newFake(t *testing.T) *fake {
	f := &fake{qcEnabled: true, tokens: map[string]string{}, pending: map[string]string{}}
	tokRe := regexp.MustCompile(`Token="([^"]*)"`)
	user := func(r *http.Request) string {
		if m := tokRe.FindStringSubmatch(r.Header.Get("Authorization")); m != nil {
			f.mu.Lock()
			defer f.mu.Unlock()
			if m[1] == "the-api-key" {
				return "*apikey*"
			}
			return f.tokens[m[1]]
		}
		return ""
	}
	issue := func(u string) string {
		f.n++
		tok := fmt.Sprintf("tok-%d", f.n)
		f.tokens[tok] = u
		return tok
	}
	mux := http.NewServeMux()
	j := func(w http.ResponseWriter, v any) { json.NewEncoder(w).Encode(v) }
	mux.HandleFunc("GET /QuickConnect/Enabled", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		j(w, f.qcEnabled)
	})
	mux.HandleFunc("POST /QuickConnect/Initiate", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.n++
		secret := fmt.Sprintf("s-%d", f.n)
		f.pending[secret] = ""
		j(w, map[string]any{"Code": fmt.Sprintf("%06d", f.n), "Secret": secret})
	})
	mux.HandleFunc("GET /QuickConnect/Connect", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		s := r.URL.Query().Get("secret")
		if f.approveAs != "" {
			f.pending[s] = f.approveAs // someone approves on their device
		}
		j(w, map[string]any{"Authenticated": f.pending[s] != ""})
	})
	mux.HandleFunc("POST /Users/AuthenticateWithQuickConnect", func(w http.ResponseWriter, r *http.Request) {
		var b struct{ Secret string }
		json.NewDecoder(r.Body).Decode(&b)
		f.mu.Lock()
		defer f.mu.Unlock()
		if u := f.pending[b.Secret]; u != "" {
			j(w, map[string]any{"AccessToken": issue(u)})
			return
		}
		http.Error(w, "no", http.StatusUnauthorized)
	})
	mux.HandleFunc("POST /Users/AuthenticateByName", func(w http.ResponseWriter, r *http.Request) {
		var b struct{ Username, Pw string }
		json.NewDecoder(r.Body).Decode(&b)
		f.mu.Lock()
		defer f.mu.Unlock()
		if _, ok := users[b.Username]; ok && b.Pw == "pw" {
			j(w, map[string]any{"AccessToken": issue(b.Username)})
			return
		}
		http.Error(w, "bad", http.StatusUnauthorized)
	})
	mux.HandleFunc("GET /Users/Me", func(w http.ResponseWriter, r *http.Request) {
		u := user(r)
		if u == "" || u == "*apikey*" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		j(w, map[string]any{"Id": "id-" + u, "Name": u, "Policy": map[string]any{"IsAdministrator": users[u], "EnableContentDeletion": users[u]}})
	})
	mux.HandleFunc("GET /Users", func(w http.ResponseWriter, r *http.Request) {
		if user(r) != "*apikey*" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		j(w, []map[string]string{{"Name": "bloom", "Id": "id-bloom"}})
	})
	mux.HandleFunc("POST /Sessions/Logout", func(w http.ResponseWriter, r *http.Request) {
		if m := tokRe.FindStringSubmatch(r.Header.Get("Authorization")); m != nil {
			f.mu.Lock()
			delete(f.tokens, m[1])
			f.mu.Unlock()
		}
		w.WriteHeader(http.StatusNoContent)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fake) valid(tok string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.tokens[tok]
	return ok
}

func (f *fake) open() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.tokens)
}

type env struct {
	f     *fake
	store secrets.Store
	out   *bytes.Buffer
	u     config.Upstream
}

func newEnv(t *testing.T) *env {
	f := newFake(t)
	store, err := secrets.OpenFileStore(filepath.Join(t.TempDir(), "secrets"))
	if err != nil {
		t.Fatal(err)
	}
	return &env{f: f, store: store, out: &bytes.Buffer{}, u: config.Upstream{Name: "friend", URL: f.srv.URL}}
}

func (e *env) pair(mode Mode, prompts ...string) error {
	i := 0
	return Pair(context.Background(), Options{Upstream: e.u, Store: e.store, Mode: mode, Out: e.out,
		Poll: time.Millisecond, Timeout: 200 * time.Millisecond,
		Prompt: func(string, bool) (string, error) { i++; return prompts[i-1], nil }})
}

func (e *env) stored(t *testing.T) upstream.Secret {
	t.Helper()
	s, err := secrets.Load(e.store, "friend")
	if err != nil {
		t.Fatalf("no stored secret: %v", err)
	}
	return s
}

func TestQuickConnectPairing(t *testing.T) {
	e := newEnv(t)
	e.f.approveAs = "bloom"
	if err := e.pair(QuickConnect); err != nil {
		t.Fatal(err)
	}
	s := e.stored(t)
	if s.Kind != upstream.KindToken || !e.f.valid(s.Value) {
		t.Errorf("stored %+v", s)
	}
	out := e.out.String()
	for _, want := range []string{"enter this code:  0000", `Paired "friend" as "bloom" (not an administrator)`, "Dashboard → Devices"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}

	// Pairing again replaces and revokes the old token.
	if err := e.pair(QuickConnect); err != nil {
		t.Fatal(err)
	}
	if e.f.valid(s.Value) || e.stored(t).Value == s.Value {
		t.Error("re-pairing must revoke the previous token")
	}
}

func TestAdminRefused(t *testing.T) {
	e := newEnv(t)
	e.f.approveAs = "boss"
	err := e.pair(QuickConnect)
	if err == nil || !strings.Contains(err.Error(), "is an administrator") {
		t.Fatalf("err = %v", err)
	}
	if _, err := secrets.Load(e.store, "friend"); !errors.Is(err, upstream.ErrNotPaired) {
		t.Error("an admin credential was stored")
	}
	if e.f.open() != 0 {
		t.Error("the refused admin token was not revoked")
	}

	e.u.AllowAdmin = true
	if err := e.pair(QuickConnect); err != nil {
		t.Fatalf("with allow_admin: %v", err)
	}
	if !strings.Contains(e.out.String(), "may delete media") {
		t.Error("expected a note about the account's delete permission")
	}
}

func TestQuickConnectDisabledAndTimeout(t *testing.T) {
	e := newEnv(t)
	e.f.qcEnabled = false
	if err := e.pair(QuickConnect); err == nil || !strings.Contains(err.Error(), "--password") {
		t.Errorf("disabled: err = %v", err)
	}
	e.f.qcEnabled = true // nobody approves
	if err := e.pair(QuickConnect); err == nil || !strings.Contains(err.Error(), "not approved") {
		t.Errorf("timeout: err = %v", err)
	}
}

func TestPasswordPairing(t *testing.T) {
	e := newEnv(t)
	if err := e.pair(Password, "bloom", "pw"); err != nil {
		t.Fatal(err)
	}
	s := e.stored(t)
	if s.Kind != upstream.KindToken || !e.f.valid(s.Value) || strings.Contains(s.Value, "pw") {
		t.Errorf("stored %+v", s)
	}
	if err := e.pair(Password, "bloom", "wrong"); err == nil {
		t.Error("bad password accepted")
	}
}

func TestAPIKeyPairing(t *testing.T) {
	e := newEnv(t)
	if err := e.pair(APIKey, "the-api-key"); err == nil || !strings.Contains(err.Error(), "allow_admin") {
		t.Errorf("api key without allow_admin: %v", err)
	}
	e.u.User, e.u.AllowAdmin = "bloom", true
	if err := e.pair(APIKey, "the-api-key"); err != nil {
		t.Fatal(err)
	}
	if s := e.stored(t); s.Kind != upstream.KindAPIKey || s.User != "bloom" {
		t.Errorf("stored %+v", s)
	}
}

func TestUnpair(t *testing.T) {
	e := newEnv(t)
	e.f.approveAs = "bloom"
	e.pair(QuickConnect)
	tok := e.stored(t).Value
	if err := Unpair(context.Background(), e.u, e.store, e.out); err != nil {
		t.Fatal(err)
	}
	if e.f.valid(tok) {
		t.Error("token still valid after unpair")
	}
	if _, err := secrets.Load(e.store, "friend"); !errors.Is(err, upstream.ErrNotPaired) {
		t.Error("secret still stored")
	}
	if err := Unpair(context.Background(), e.u, e.store, e.out); err == nil {
		t.Error("unpairing twice should report not paired")
	}
}
