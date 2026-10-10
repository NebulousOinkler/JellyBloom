package auth

import (
	"context"
	"errors"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"github.com/NebulousOinkler/JellyBloom/internal/config"
	"github.com/NebulousOinkler/JellyBloom/internal/index"
)

func TestParseRequest(t *testing.T) {
	cases := []struct {
		name    string
		headers map[string]string
		query   string
		want    ClientInfo
	}{
		{"mediabrowser header", map[string]string{"Authorization": `MediaBrowser Client="Jellyfin Web", Device="Fire%20fox", DeviceId="d1", Version="10.10.7", Token="t1"`},
			"", ClientInfo{Client: "Jellyfin Web", Device: "Fire fox", DeviceID: "d1", Version: "10.10.7", Token: "t1"}},
		{"emby authorization header", map[string]string{"X-Emby-Authorization": `Emby Client="Infuse", DeviceId="d2", Token="t2"`},
			"", ClientInfo{Client: "Infuse", DeviceID: "d2", Token: "t2"}},
		{"x-emby-token", map[string]string{"X-Emby-Token": "t3"}, "", ClientInfo{Token: "t3"}},
		{"x-mediabrowser-token", map[string]string{"X-MediaBrowser-Token": "t4"}, "", ClientInfo{Token: "t4"}},
		{"api_key query", nil, "api_key=t5&deviceId=d5", ClientInfo{Token: "t5", DeviceID: "d5"}},
		{"ApiKey query", nil, "ApiKey=t6", ClientInfo{Token: "t6"}},
		{"header token wins over query", map[string]string{"X-Emby-Token": "t7"}, "api_key=other", ClientInfo{Token: "t7"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/x?"+tc.query, nil)
			for k, v := range tc.headers {
				r.Header.Set(k, v)
			}
			if got := ParseRequest(r); got != tc.want {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func newService(t *testing.T) *Service {
	t.Helper()
	idx, err := index.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { idx.Close() })
	h, _ := bcrypt.GenerateFromPassword([]byte("pw"), bcrypt.MinCost)
	s, err := New(idx.DB(), []config.User{{Name: "Me", PasswordHash: string(h)}})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSessions(t *testing.T) {
	ctx := context.Background()
	s := newService(t)
	if len(s.ServerID()) != 32 {
		t.Errorf("ServerID = %q", s.ServerID())
	}

	if _, _, err := s.Authenticate(ctx, "me", "wrong", ClientInfo{}); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("wrong password: %v", err)
	}
	if _, _, err := s.Authenticate(ctx, "nobody", "pw", ClientInfo{}); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("unknown user: %v", err)
	}
	sess, u, err := s.Authenticate(ctx, "ME", "pw", ClientInfo{DeviceID: "dev", Client: "web"})
	if err != nil || u.Name != "Me" || len(sess.Token) != 32 {
		t.Fatalf("login: %v %+v %+v", err, u, sess)
	}
	if _, u2, err := s.Validate(ctx, sess.Token); err != nil || u2.ID != u.ID {
		t.Errorf("Validate: %v", err)
	}

	// A second login from the same device replaces the first session.
	sess2, _, _ := s.Authenticate(ctx, "me", "pw", ClientInfo{DeviceID: "dev"})
	if _, _, err := s.Validate(ctx, sess.Token); !errors.Is(err, ErrUnauthorized) {
		t.Error("old session on same device still valid")
	}
	if err := s.Logout(ctx, sess2.Token); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Validate(ctx, sess2.Token); !errors.Is(err, ErrUnauthorized) {
		t.Error("session valid after logout")
	}
}
