package upstream

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/NebulousOinkler/JellyBloom/internal/config"
)

func TestLoginWithPassword(t *testing.T) {
	f := newFake(t)
	c := f.client(t, config.Upstream{Username: "me", Password: "pw"})

	if _, err := c.Views(testCtx(t)); err != nil {
		t.Fatal(err)
	}
	if got := c.UserID(); got != "user-me" {
		t.Errorf("UserID = %q, want user-me", got)
	}
	if got := f.query("userId"); got != "user-me" {
		t.Errorf("userId param = %q, want user-me", got)
	}
	f.mu.Lock()
	auth := f.lastAuth
	f.mu.Unlock()
	for _, want := range []string{`MediaBrowser Client="JellyBloom"`, `DeviceId="` + c.deviceID + `"`, `Version="` + Version + `"`, `Token="token-1"`} {
		if !strings.Contains(auth, want) {
			t.Errorf("Authorization %q missing %q", auth, want)
		}
	}
}

func TestLoginBadPassword(t *testing.T) {
	f := newFake(t)
	c := f.client(t, config.Upstream{Username: "me", Password: "wrong"})

	err := c.Login(testCtx(t))
	var se *StatusError
	if !errors.As(err, &se) || se.Code != http.StatusUnauthorized {
		t.Fatalf("err = %v, want 401 StatusError", err)
	}
}

func TestLoginWithAPIKey(t *testing.T) {
	f := newFake(t)
	c := f.client(t, config.Upstream{APIKey: "admin-key", User: "Guest"})

	if err := c.Login(testCtx(t)); err != nil {
		t.Fatal(err)
	}
	if got := c.UserID(); got != "user-guest" {
		t.Errorf("UserID = %q, want user-guest (case-insensitive match)", got)
	}
}

func TestLoginWithAPIKeyUnknownUser(t *testing.T) {
	f := newFake(t)
	c := f.client(t, config.Upstream{APIKey: "admin-key", User: "nobody"})

	if err := c.Login(testCtx(t)); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("err = %v, want user not found", err)
	}
}

func TestReloginOn401(t *testing.T) {
	f := newFake(t)
	c := f.client(t, config.Upstream{Username: "me", Password: "pw"})
	ctx := testCtx(t)

	if _, err := c.Views(ctx); err != nil {
		t.Fatal(err)
	}
	f.revokeAll()
	if _, err := c.Views(ctx); err != nil {
		t.Fatalf("after revoke: %v", err)
	}
	if n := f.get(func() int { return f.logins }); n != 2 {
		t.Errorf("logins = %d, want 2", n)
	}
}

func TestRetryOnServiceUnavailable(t *testing.T) {
	f := newFake(t)
	c := f.client(t, config.Upstream{Username: "me", Password: "pw"})
	ctx := testCtx(t)
	if err := c.Login(ctx); err != nil {
		t.Fatal(err)
	}

	f.mu.Lock()
	f.failNext = 2
	f.mu.Unlock()
	if _, err := c.Views(ctx); err != nil {
		t.Fatalf("expected success after retries: %v", err)
	}

	f.mu.Lock()
	f.failNext = 10 // more than MaxRetries
	f.mu.Unlock()
	_, err := c.Views(ctx)
	var se *StatusError
	if !errors.As(err, &se) || se.Code != http.StatusServiceUnavailable {
		t.Fatalf("err = %v, want 503 after exhausting retries", err)
	}
}

func TestNotFoundIsNotRetried(t *testing.T) {
	f := newFake(t)
	c := f.client(t, config.Upstream{Username: "me", Password: "pw"})

	err := c.get(testCtx(t), "/Nope", nil, nil)
	var se *StatusError
	if !errors.As(err, &se) || se.Code != http.StatusNotFound {
		t.Fatalf("err = %v, want 404", err)
	}
}

func TestEachItemPage(t *testing.T) {
	f := newFake(t)
	c := f.client(t, config.Upstream{Username: "me", Password: "pw"})

	var pages, items int
	q := ItemsQuery{Recursive: true, IncludeItemTypes: []string{"Movie", "Episode"}, Fields: []string{"ProviderIds"}}
	err := c.EachItemPage(testCtx(t), q, 10, func(p *QueryResult) error {
		pages++
		items += len(p.Items)
		if p.TotalRecordCount != 25 {
			t.Errorf("TotalRecordCount = %d", p.TotalRecordCount)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if pages != 3 || items != 25 {
		t.Errorf("pages=%d items=%d, want 3 and 25", pages, items)
	}
	for k, want := range map[string]string{"recursive": "true", "includeItemTypes": "Movie,Episode", "fields": "ProviderIds", "limit": "10", "startIndex": "20"} {
		if got := f.query(k); got != want {
			t.Errorf("last query %s = %q, want %q", k, got, want)
		}
	}
}

func TestEachArtistPage(t *testing.T) {
	f := newFake(t)
	c := f.client(t, config.Upstream{Username: "me", Password: "pw"})

	items := 0
	err := c.EachArtistPage(testCtx(t), ItemsQuery{ParentID: "lib-music"}, 20, func(p *QueryResult) error {
		items += len(p.Items)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if items != 25 || f.query("parentId") != "lib-music" {
		t.Errorf("items=%d parentId=%q", items, f.query("parentId"))
	}
}

func TestEachItemPageStopsOnCallbackError(t *testing.T) {
	f := newFake(t)
	c := f.client(t, config.Upstream{Username: "me", Password: "pw"})

	stop := errors.New("stop")
	calls := 0
	err := c.EachItemPage(testCtx(t), ItemsQuery{}, 10, func(*QueryResult) error { calls++; return stop })
	if !errors.Is(err, stop) || calls != 1 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
}

func TestPublicInfoAndPing(t *testing.T) {
	f := newFake(t)
	c := f.client(t, config.Upstream{Username: "me", Password: "pw"})
	ctx := testCtx(t)

	info, err := c.PublicInfo(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if info.Id != "server-1" || info.Version != "10.10.7" {
		t.Errorf("info = %+v", info)
	}
	if _, err := c.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	if n := f.get(func() int { return f.logins }); n != 0 {
		t.Errorf("public endpoints logged in %d times", n)
	}

	f.srv.Close()
	if _, err := c.Ping(ctx); err == nil {
		t.Error("Ping succeeded against a closed server")
	}
}

func TestBaseURLWithSubpath(t *testing.T) {
	c, err := New(config.Upstream{Name: "x", URL: "https://example.com/jellyfin/"}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if got := c.url("/Items", nil); got != "https://example.com/jellyfin/Items" {
		t.Errorf("url = %q", got)
	}
	if got := c.socketURL("t"); !strings.HasPrefix(got, "wss://example.com/jellyfin/socket?") {
		t.Errorf("socketURL = %q", got)
	}
	if _, err := New(config.Upstream{Name: "x", URL: "ftp://example.com"}, Options{}); err == nil {
		t.Error("expected error for non-http url")
	}
}

func TestListen(t *testing.T) {
	f := newFake(t)
	c := f.client(t, config.Upstream{Username: "me", Password: "pw"})
	ctx, cancel := context.WithCancel(testCtx(t))

	events := make(chan Event, 16)
	done := make(chan error, 1)
	go func() { done <- c.Listen(ctx, func(e Event) { events <- e }) }()

	next := func(want EventType) Event {
		t.Helper()
		for {
			select {
			case e := <-events:
				if e.Type == want {
					return e
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("timed out waiting for %s", want)
			}
		}
	}

	s := f.nextSocket(t)
	next(EventConnected)

	// Upstream library change is parsed and delivered.
	err := wsjson.Write(ctx, s.conn, map[string]any{
		"MessageType": "LibraryChanged",
		"Data":        map[string]any{"ItemsAdded": []string{"a1"}, "ItemsRemoved": []string{"r1", "r2"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	e := next(EventLibraryChanged)
	if e.Upstream != "fake" || len(e.Library.ItemsAdded) != 1 || len(e.Library.ItemsRemoved) != 2 {
		t.Errorf("LibraryChanged event = %+v", e.Library)
	}

	// Other messages pass through raw.
	if err := wsjson.Write(ctx, s.conn, map[string]any{"MessageType": "UserDataChanged", "Data": map[string]any{}}); err != nil {
		t.Fatal(err)
	}
	if e := next(EventMessage); e.MessageType != "UserDataChanged" {
		t.Errorf("MessageType = %q", e.MessageType)
	}

	// KeepAlives are sent at half the ForceKeepAlive interval (0.2s).
	deadline := time.Now().Add(2 * time.Second)
	for f.get(func() int { return f.keepAlives }) < 2 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if n := f.get(func() int { return f.keepAlives }); n < 2 {
		t.Errorf("keepAlives = %d, want >= 2", n)
	}

	// A dropped connection reconnects, re-logging in if the token was revoked.
	f.revokeAll()
	s.conn.Close(websocket.StatusGoingAway, "restart")
	next(EventDisconnected)
	f.nextSocket(t)
	next(EventConnected)
	if n := f.get(func() int { return f.logins }); n != 2 {
		t.Errorf("logins = %d, want 2 after revoked token", n)
	}

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Listen returned %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Listen did not return after cancel")
	}
}
