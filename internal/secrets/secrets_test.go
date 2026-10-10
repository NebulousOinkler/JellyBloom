package secrets

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zalando/go-keyring"

	"github.com/NebulousOinkler/JellyBloom/internal/config"
	"github.com/NebulousOinkler/JellyBloom/internal/upstream"
)

func TestFileStore(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "secrets")
	s, err := OpenFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(dir); fi.Mode().Perm() != 0o700 {
		t.Errorf("dir mode = %o", fi.Mode().Perm())
	}
	if _, err := s.Get("upstream-a"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing key: %v", err)
	}
	if err := s.Set("upstream-a", []byte("v1")); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(filepath.Join(dir, "upstream-a")); fi.Mode().Perm() != 0o600 {
		t.Errorf("file mode = %o", fi.Mode().Perm())
	}
	if v, _ := s.Get("upstream-a"); string(v) != "v1" {
		t.Errorf("Get = %q", v)
	}
	if err := s.Set("../escape", []byte("x")); err == nil {
		t.Error("path traversal key accepted")
	}
	if err := s.Delete("upstream-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get("upstream-a"); !errors.Is(err, ErrNotFound) {
		t.Error("not deleted")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("leftover files: %v", entries)
	}
}

func TestFileStoreRefusesLoosePermissions(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "secrets")
	s, _ := OpenFileStore(dir)
	s.Set("upstream-a", []byte("v"))
	os.Chmod(filepath.Join(dir, "upstream-a"), 0o644)
	if _, err := s.Get("upstream-a"); err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Errorf("Get on world-readable secret: %v", err)
	}
	if _, err := OpenFileStore(dir); err == nil {
		t.Error("OpenFileStore accepted a world-readable secret")
	}
	os.Chmod(filepath.Join(dir, "upstream-a"), 0o600)
	os.Chmod(dir, 0o755)
	if _, err := OpenFileStore(dir); err == nil || !strings.Contains(err.Error(), "chmod 700") {
		t.Errorf("OpenFileStore on 755 dir: %v", err)
	}
}

func TestKeychainStore(t *testing.T) {
	keyring.MockInit()
	k := KeychainStore{service: keychainService}
	if _, err := k.Get("upstream-a"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing: %v", err)
	}
	k.Set("upstream-a", []byte("v"))
	if v, _ := k.Get("upstream-a"); string(v) != "v" {
		t.Errorf("Get = %q", v)
	}
	if err := k.Delete("upstream-a"); err != nil {
		t.Fatal(err)
	}
	if err := k.Delete("upstream-a"); err != nil {
		t.Errorf("deleting a missing secret: %v", err)
	}
}

func TestCredentials(t *testing.T) {
	ctx := context.Background()
	s, _ := OpenFileStore(filepath.Join(t.TempDir(), "secrets"))

	c := Credentials(s, config.Upstream{Name: "a"})
	if _, err := c.Load(ctx); !errors.Is(err, upstream.ErrNotPaired) {
		t.Errorf("unpaired: %v", err)
	}
	c.Save(ctx, upstream.Secret{Kind: upstream.KindToken, Value: "tok"})
	if sec, err := c.Load(ctx); err != nil || sec.Value != "tok" {
		t.Errorf("Load = %+v, %v", sec, err)
	}
	if c.Password != nil {
		t.Error("no password_file, but a password source")
	}

	pw := filepath.Join(t.TempDir(), "pw")
	os.WriteFile(pw, []byte("hunter2\n"), 0o600)
	c = Credentials(s, config.Upstream{Name: "b", Username: "me", PasswordFile: pw})
	if u, p, err := c.Password(); err != nil || u != "me" || p != "hunter2" {
		t.Errorf("password_file = %q %q %v", u, p, err)
	}

	cmd := Credentials(s, config.Upstream{Name: "c", User: "svc", AllowAdmin: true, TokenCommand: []string{"echo", "  key123  "}})
	sec, err := cmd.Load(ctx)
	if err != nil || sec.Value != "key123" || sec.Kind != upstream.KindAPIKey || sec.User != "svc" || !cmd.AllowAdmin || cmd.Save != nil {
		t.Errorf("token_command = %+v %v (AllowAdmin %v)", sec, err, cmd.AllowAdmin)
	}
	bad := Credentials(s, config.Upstream{Name: "d", TokenCommand: []string{"sh", "-c", "echo secret-in-stderr >&2; exit 1"}})
	if _, err := bad.Load(ctx); err == nil || strings.Contains(err.Error(), "secret-in-stderr") {
		t.Errorf("failing token_command error = %v (must not include stderr)", err)
	}
}
