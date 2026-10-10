package upstream

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestStoredTokenLogin(t *testing.T) {
	f := newFake(t)
	sec := Secret{Kind: KindToken, Value: f.issue("me", "jb")}
	c := f.client(t, storeCreds(&sec))
	if err := c.Login(testCtx(t)); err != nil {
		t.Fatal(err)
	}
	if c.UserID() != "user-me" {
		t.Errorf("UserID = %q", c.UserID())
	}
}

func TestNotPaired(t *testing.T) {
	f := newFake(t)
	var sec Secret
	err := f.client(t, storeCreds(&sec)).Login(testCtx(t))
	var ce *CredentialError
	if !errors.As(err, &ce) || !errors.Is(err, ErrNotPaired) {
		t.Fatalf("err = %v, want CredentialError(ErrNotPaired)", err)
	}
}

func TestAdminAccountsRefusedUnlessAllowed(t *testing.T) {
	f := newFake(t)
	sec := Secret{Kind: KindToken, Value: f.issue("boss", "jb")}
	creds := storeCreds(&sec)
	if err := f.client(t, creds).Login(testCtx(t)); !errors.Is(err, ErrAdmin) {
		t.Fatalf("admin token: err = %v, want ErrAdmin", err)
	}
	creds.AllowAdmin = true
	if err := f.client(t, creds).Login(testCtx(t)); err != nil {
		t.Fatalf("admin token with AllowAdmin: %v", err)
	}

	// API keys are always admin-level.
	k := apiCreds("admin-key", "me")
	k.AllowAdmin = false
	if err := f.client(t, k).Login(testCtx(t)); !errors.Is(err, ErrAdmin) {
		t.Errorf("api key without AllowAdmin: err = %v", err)
	}

	// An admin password login is refused and its session revoked.
	p := pwCreds("boss", "pw")
	if err := f.client(t, p).Login(testCtx(t)); !errors.Is(err, ErrAdmin) {
		t.Errorf("admin password: err = %v", err)
	}
	f.mu.Lock()
	for tok, uid := range f.tokens {
		if uid == "user-boss" && tok != sec.Value {
			t.Errorf("refused admin session %s left open", tok)
		}
	}
	f.mu.Unlock()
}

func TestRevokedToken(t *testing.T) {
	ctx := testCtx(t)
	f := newFake(t)
	sec := Secret{Kind: KindToken, Value: f.issue("me", "jb")}
	c := f.client(t, storeCreds(&sec))
	if err := c.Login(ctx); err != nil {
		t.Fatal(err)
	}
	f.revokeAll()
	if _, err := c.Views(ctx); !errors.Is(err, ErrRevoked) {
		t.Fatalf("after revoke: err = %v, want ErrRevoked", err)
	}

	// With a fallback password, a rejected token is replaced and saved.
	creds := storeCreds(&sec)
	creds.Password = pwCreds("me", "pw").Password
	c = f.client(t, creds)
	if _, err := c.Views(ctx); err != nil {
		t.Fatalf("password fallback: %v", err)
	}
	if !f.valid(sec.Value) {
		t.Error("new token was not saved")
	}
}

func TestPairingWithQuickConnect(t *testing.T) {
	ctx := testCtx(t)
	f := newFake(t)
	c := f.client(t, Credentials{})
	if !c.QuickConnectEnabled(ctx) {
		t.Fatal("quick connect should be enabled")
	}
	q, err := c.StartPairing(ctx)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(30 * time.Millisecond)
		f.approve(q.Code, "me") // someone approves the code in their Jellyfin app
	}()
	sec, err := c.FinishPairing(ctx, q, 5*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	a, err := c.Account(ctx, sec)
	if err != nil || a.ID != "user-me" || a.Admin {
		t.Fatalf("paired account = %+v, %v", a, err)
	}
	if err := c.Revoke(ctx, sec); err != nil || f.valid(sec.Value) {
		t.Errorf("Revoke: err=%v, still valid=%v", err, f.valid(sec.Value))
	}

	expired, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	q2, _ := c.StartPairing(ctx)
	if _, err := c.FinishPairing(expired, q2, 5*time.Millisecond); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("unapproved pairing: err = %v", err)
	}
}

func TestPasswordSecretKeepsNoPassword(t *testing.T) {
	ctx := testCtx(t)
	f := newFake(t)
	c := f.client(t, Credentials{})
	sec, err := c.PasswordSecret(ctx, "me", "pw")
	if err != nil || sec.Kind != KindToken || !f.valid(sec.Value) {
		t.Fatalf("PasswordSecret = %+v, %v", sec, err)
	}
}

func TestDeviceTokensViaQuickConnect(t *testing.T) {
	ctx := testCtx(t)
	f := newFake(t)
	sec := Secret{Kind: KindToken, Value: f.issue("me", "jb")}
	c := f.client(t, storeCreds(&sec))

	d1 := Device{ID: DeviceID("fake", "user1", "phone"), Name: "Phone", Client: "Swiftfin", Version: "1.0"}
	d2 := Device{ID: DeviceID("fake", "user1", "tv"), Name: "TV"}
	if d1.ID == d2.ID || len(d1.ID) != 32 || DeviceID("fake", "user1", "phone") != d1.ID {
		t.Fatal("DeviceID not stable/distinct")
	}
	t1, per1, err := c.DeviceToken(ctx, d1)
	if err != nil || !per1 {
		t.Fatalf("d1: %v perDevice=%v", err, per1)
	}
	t2, _, _ := c.DeviceToken(ctx, d2)
	again, _, _ := c.DeviceToken(ctx, d1)
	if t1 == t2 || t1 != again || t1 == sec.Value {
		t.Errorf("tokens: d1=%s d2=%s again=%s root=%s", t1, t2, again, sec.Value)
	}
	f.mu.Lock()
	dev1, user1 := f.devices[t1], f.tokens[t1]
	f.mu.Unlock()
	if dev1 != d1.ID || user1 != "user-me" {
		t.Errorf("d1 token issued to device %q user %q", dev1, user1)
	}
	if n := f.get(func() int { return f.logins }); n != 3 { // the issued root + two Quick Connect devices
		t.Errorf("logins = %d, want 3 (no password logins)", n)
	}

	// A rejected device token is replaced.
	f.mu.Lock()
	delete(f.tokens, t1)
	f.mu.Unlock()
	if err := c.ReauthDevice(ctx, d1, t1); err != nil {
		t.Fatal(err)
	}
	if t3, _, _ := c.DeviceToken(ctx, d1); t3 == t1 || !f.valid(t3) {
		t.Error("ReauthDevice did not mint a new token")
	}
}

// A hung upstream must not hold a client's requests up while JellyBloom
// tries to give the client its own session: after a bounded wait the
// request uses JellyBloom's session, and minting is retried later.
func TestDeviceTokenWhenUpstreamHangs(t *testing.T) {
	defer func(t, r time.Duration) { mintTimeout, mintRetry = t, r }(mintTimeout, mintRetry)
	mintTimeout, mintRetry = 200*time.Millisecond, time.Hour
	ctx := testCtx(t)
	f := newFake(t)
	sec := Secret{Kind: KindToken, Value: f.issue("me", "jb")}
	c := f.client(t, storeCreds(&sec))
	d := Device{ID: DeviceID("fake", "user1", "phone")}

	f.mu.Lock()
	f.qcHang = true
	f.mu.Unlock()
	start := time.Now()
	tok, per, err := c.DeviceToken(ctx, d)
	if err != nil || per || tok != sec.Value || time.Since(start) > 2*time.Second {
		t.Fatalf("hung upstream: %q per=%v err=%v after %s", tok, per, err, time.Since(start))
	}
	// Within the retry interval: no new attempt, no wait.
	start = time.Now()
	if tok, per, _ := c.DeviceToken(ctx, d); per || tok != sec.Value || time.Since(start) > 50*time.Millisecond {
		t.Errorf("second call: per=%v after %s", per, time.Since(start))
	}
	// Once the interval passes and the upstream recovers, the device gets
	// its own session.
	f.mu.Lock()
	f.qcHang = false
	f.mu.Unlock()
	c.deviceSession(d.ID).retryAt = time.Time{}
	if tok, per, err := c.DeviceToken(ctx, d); err != nil || !per || tok == sec.Value {
		t.Errorf("after recovery: per=%v err=%v", per, err)
	}
}

func TestDeviceTokensFallbacks(t *testing.T) {
	ctx := testCtx(t)
	f := newFake(t)
	f.qcEnabled = false
	d := Device{ID: DeviceID("fake", "u", "phone"), Name: "Phone"}

	// Quick Connect off, password available: the device logs in with it.
	p := f.client(t, pwCreds("me", "pw"))
	tok, per, err := p.DeviceToken(ctx, d)
	f.mu.Lock()
	dev := f.devices[tok]
	f.mu.Unlock()
	if err != nil || !per || dev != d.ID {
		t.Errorf("password device: per=%v device=%q err=%v", per, dev, err)
	}

	// Neither: clients share JellyBloom's own session, and say so.
	sec := Secret{Kind: KindToken, Value: f.issue("me", "jb")}
	s := f.client(t, storeCreds(&sec))
	tok, per, err = s.DeviceToken(ctx, d)
	if err != nil || per || tok != sec.Value || !s.SharedDevices() {
		t.Errorf("shared fallback: tok=%s per=%v shared=%v err=%v", tok, per, s.SharedDevices(), err)
	}

	// API keys serve every device (the device travels in the header).
	k := f.client(t, apiCreds("admin-key", "me"))
	if tok, per, err := k.DeviceToken(ctx, d); err != nil || !per || tok != "admin-key" {
		t.Errorf("api key device token = %q per=%v err=%v", tok, per, err)
	}
	if h := k.DeviceAuthHeader(d, "admin-key"); !contains(h, `DeviceId="`+d.ID+`"`) || !contains(h, `Device="Phone"`) {
		t.Errorf("device header = %s", h)
	}
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }

// TestRevokedUpstreamSurfacesCredentialError covers an upstream admin
// revoking JellyBloom's paired session: requests must report "re-pair", not
// raw HTTP errors.
func TestRevokedUpstreamSurfacesCredentialError(t *testing.T) {
	ctx := testCtx(t)
	f := newFake(t)
	sec := Secret{Kind: KindToken, Value: f.issue("me", "jb")}
	c := f.client(t, storeCreds(&sec))
	if err := c.Login(ctx); err != nil {
		t.Fatal(err)
	}
	f.revokeAll()
	_, _, err := c.DeviceToken(ctx, Device{ID: DeviceID("fake", "u", "new-device")})
	var ce *CredentialError
	if !errors.As(err, &ce) || !errors.Is(err, ErrRevoked) {
		t.Errorf("DeviceToken after revocation: %v, want CredentialError(ErrRevoked)", err)
	}
}
