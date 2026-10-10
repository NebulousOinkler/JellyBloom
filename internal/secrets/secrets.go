// Package secrets stores upstream credentials outside the config file: in
// owner-only files under the data directory (the default) or in the OS
// keychain. It also builds upstream.Credentials from the config, a store,
// and the optional token_command / password_file sources.
package secrets

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/zalando/go-keyring"

	"github.com/NebulousOinkler/JellyBloom/internal/config"
	"github.com/NebulousOinkler/JellyBloom/internal/upstream"
)

// ErrNotFound means no secret is stored under the key.
var ErrNotFound = errors.New("secret not found")

// Store keeps named secrets.
type Store interface {
	Get(key string) ([]byte, error)
	Set(key string, value []byte) error
	Delete(key string) error
	Describe() string // where secrets live, for messages
}

var keyPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

func checkKey(key string) error {
	if !keyPattern.MatchString(key) {
		return fmt.Errorf("secrets: invalid key %q", key)
	}
	return nil
}

// FileStore keeps each secret in its own 0600 file in a 0700 directory.
type FileStore struct{ dir string }

// OpenFileStore opens (creating) dir. It refuses directories or files that
// other users can access, so a misconfigured host fails loudly.
func OpenFileStore(dir string) (*FileStore, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := ownerOnly(dir); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if err := ownerOnly(filepath.Join(dir, e.Name())); err != nil {
			return nil, err
		}
	}
	return &FileStore{dir: dir}, nil
}

func ownerOnly(path string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	if fi.Mode().Perm()&0o077 != 0 {
		want := "600"
		if fi.IsDir() {
			want = "700"
		}
		return fmt.Errorf("secrets: %s is accessible by other users (mode %o); run: chmod %s %q", path, fi.Mode().Perm(), want, path)
	}
	return nil
}

func (s *FileStore) path(key string) (string, error) {
	if err := checkKey(key); err != nil {
		return "", err
	}
	return filepath.Join(s.dir, key), nil
}

func (s *FileStore) Get(key string) ([]byte, error) {
	p, err := s.path(key)
	if err != nil {
		return nil, err
	}
	if err := ownerOnly(p); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return os.ReadFile(p)
}

// Set writes atomically (temp file + rename), never exposing a partial
// secret or a moment of wider permissions.
func (s *FileStore) Set(key string, value []byte) error {
	p, err := s.path(key)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(s.dir, "."+key+".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(value); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), p)
}

func (s *FileStore) Delete(key string) error {
	p, err := s.path(key)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

func (s *FileStore) Describe() string { return "file store " + s.dir }

// KeychainStore keeps secrets in the OS keychain (macOS Keychain, Secret
// Service on Linux, Credential Manager on Windows).
type KeychainStore struct{ service string }

const keychainService = "JellyBloom"

func (k KeychainStore) Get(key string) ([]byte, error) {
	if err := checkKey(key); err != nil {
		return nil, err
	}
	v, err := keyring.Get(k.service, key)
	if errors.Is(err, keyring.ErrNotFound) {
		return nil, ErrNotFound
	}
	return []byte(v), err
}

func (k KeychainStore) Set(key string, value []byte) error {
	if err := checkKey(key); err != nil {
		return err
	}
	return keyring.Set(k.service, key, string(value))
}

func (k KeychainStore) Delete(key string) error {
	if err := checkKey(key); err != nil {
		return err
	}
	if err := keyring.Delete(k.service, key); err != nil && !errors.Is(err, keyring.ErrNotFound) {
		return err
	}
	return nil
}

func (k KeychainStore) Describe() string { return "OS keychain (service " + k.service + ")" }

// Open returns the store the config selects.
func Open(cfg *config.Config) (Store, error) {
	if cfg.SecretStore == "keychain" {
		return KeychainStore{service: keychainService}, nil
	}
	return OpenFileStore(filepath.Join(cfg.DataDir, "secrets"))
}

func upstreamKey(name string) string { return "upstream-" + name }

// Load returns an upstream's stored secret, or upstream.ErrNotPaired.
func Load(s Store, name string) (upstream.Secret, error) {
	raw, err := s.Get(upstreamKey(name))
	if errors.Is(err, ErrNotFound) {
		return upstream.Secret{}, upstream.ErrNotPaired
	}
	if err != nil {
		return upstream.Secret{}, err
	}
	var sec upstream.Secret
	if err := json.Unmarshal(raw, &sec); err != nil || sec.Value == "" {
		return upstream.Secret{}, fmt.Errorf("secrets: stored credential for %s is unreadable; re-pair it", name)
	}
	return sec, nil
}

// Save stores an upstream's secret.
func Save(s Store, name string, sec upstream.Secret) error {
	raw, err := json.Marshal(sec)
	if err != nil {
		return err
	}
	return s.Set(upstreamKey(name), raw)
}

// Delete removes an upstream's secret.
func Delete(s Store, name string) error { return s.Delete(upstreamKey(name)) }

// Credentials builds an upstream's credential sources: token_command if
// configured, else the store; plus password_file as the fallback.
func Credentials(s Store, u config.Upstream) upstream.Credentials {
	c := upstream.Credentials{AllowAdmin: u.AllowAdmin}
	if len(u.TokenCommand) > 0 {
		c.Load = func(ctx context.Context) (upstream.Secret, error) {
			v, err := RunTokenCommand(ctx, u.TokenCommand)
			if err != nil {
				return upstream.Secret{}, err
			}
			if u.User != "" {
				return upstream.Secret{Kind: upstream.KindAPIKey, Value: v, User: u.User}, nil
			}
			return upstream.Secret{Kind: upstream.KindToken, Value: v}, nil
		}
	} else {
		c.Load = func(context.Context) (upstream.Secret, error) { return Load(s, u.Name) }
		c.Save = func(_ context.Context, sec upstream.Secret) error { return Save(s, u.Name, sec) }
	}
	if u.PasswordFile != "" {
		c.Password = func() (string, string, error) {
			pw, err := ReadPasswordFile(u.PasswordFile)
			return u.Username, pw, err
		}
	}
	return c
}

// RunTokenCommand runs argv (no shell) and returns its trimmed stdout. Its
// stderr is not included in errors, since password managers may print
// sensitive context there.
func RunTokenCommand(ctx context.Context, argv []string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("token_command %s failed: %w", argv[0], err)
	}
	v := strings.TrimSpace(out.String())
	if v == "" {
		return "", fmt.Errorf("token_command %s printed nothing", argv[0])
	}
	return v, nil
}

// ReadPasswordFile reads a password file (trailing newline trimmed).
func ReadPasswordFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(b), "\r\n"), nil
}

// LoosePermissions reports a warning if path can be read by other users.
func LoosePermissions(path string) string {
	fi, err := os.Stat(path)
	if err != nil || fi.Mode().Perm()&0o077 == 0 {
		return ""
	}
	return fmt.Sprintf("%s is readable by other users (mode %o); consider chmod 600", path, fi.Mode().Perm())
}
