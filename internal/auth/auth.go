// Package auth manages JellyBloom's own users and client sessions. Clients
// authenticate to JellyBloom exactly as they would to Jellyfin; upstream
// credentials are never exposed to them.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha1"
	"database/sql"
	"encoding/hex"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/NebulousOinkler/JellyBloom/internal/config"
)

const schema = `
CREATE TABLE IF NOT EXISTS settings (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS sessions (
	token       TEXT PRIMARY KEY,
	user_id     TEXT NOT NULL,
	device_id   TEXT NOT NULL,
	device_name TEXT NOT NULL,
	client      TEXT NOT NULL,
	version     TEXT NOT NULL,
	created     INTEGER NOT NULL,
	last_seen   INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS sessions_device ON sessions (user_id, device_id);
`

// ClientInfo is what a client sends to identify and authenticate itself.
type ClientInfo struct {
	Client   string
	Device   string
	DeviceID string
	Version  string
	Token    string
}

var headerParam = regexp.MustCompile(`(\w+)="([^"]*)"`)

// ParseRequest extracts client identity and token from every place Jellyfin
// clients send them: the Authorization / X-Emby-Authorization MediaBrowser
// header, X-Emby-Token, X-MediaBrowser-Token, and the api_key/ApiKey query.
func ParseRequest(r *http.Request) ClientInfo {
	var ci ClientInfo
	for _, h := range []string{"Authorization", "X-Emby-Authorization"} {
		v := r.Header.Get(h)
		scheme, rest, _ := strings.Cut(strings.TrimSpace(v), " ")
		if !strings.EqualFold(scheme, "MediaBrowser") && !strings.EqualFold(scheme, "Emby") {
			continue
		}
		for _, m := range headerParam.FindAllStringSubmatch(rest, -1) {
			val, err := url.QueryUnescape(m[2])
			if err != nil {
				val = m[2]
			}
			switch strings.ToLower(m[1]) {
			case "client":
				ci.Client = val
			case "device":
				ci.Device = val
			case "deviceid":
				ci.DeviceID = val
			case "version":
				ci.Version = val
			case "token":
				ci.Token = val
			}
		}
		break
	}
	if ci.Token == "" {
		ci.Token = r.Header.Get("X-Emby-Token")
	}
	if ci.Token == "" {
		ci.Token = r.Header.Get("X-MediaBrowser-Token")
	}
	q := r.URL.Query()
	for _, k := range []string{"api_key", "ApiKey", "apiKey"} {
		if ci.Token == "" {
			ci.Token = q.Get(k)
		}
	}
	if ci.DeviceID == "" {
		ci.DeviceID = q.Get("deviceId")
	}
	return ci
}

// User is a JellyBloom account.
type User struct {
	ID    string
	Name  string
	Admin bool
	hash  []byte
}

// Session is a logged-in client.
type Session struct {
	Token      string
	UserID     string
	DeviceID   string
	DeviceName string
	Client     string
	Version    string
	Created    time.Time
	LastSeen   time.Time
}

// ErrUnauthorized is returned for bad credentials or tokens.
var ErrUnauthorized = errors.New("auth: unauthorized")

// Service authenticates users and tracks sessions. It is safe for concurrent use.
type Service struct {
	db       *sql.DB
	serverID string
	users    map[string]*User // by ID
	byName   map[string]*User // by lowercased name
	dummy    []byte           // hash compared for unknown users, to equalize timing

	mu       sync.Mutex
	lastSeen map[string]time.Time // token -> last persisted last_seen
}

// New loads users from config and opens session storage in db.
func New(db *sql.DB, users []config.User) (*Service, error) {
	if _, err := db.Exec(schema); err != nil {
		return nil, err
	}
	s := &Service{db: db, users: map[string]*User{}, byName: map[string]*User{}, lastSeen: map[string]time.Time{}}
	for _, u := range users {
		usr := &User{ID: userID(u.Name), Name: u.Name, Admin: u.Admin, hash: []byte(u.PasswordHash)}
		s.users[usr.ID] = usr
		s.byName[strings.ToLower(u.Name)] = usr
	}
	var err error
	if s.dummy, err = bcrypt.GenerateFromPassword([]byte("dummy"), bcrypt.MinCost); err != nil {
		return nil, err
	}
	if s.serverID, err = s.setting("server_id", newToken); err != nil {
		return nil, err
	}
	return s, nil
}

// userID derives a stable 32-hex user ID from the name.
func userID(name string) string {
	sum := sha1.Sum([]byte("jellybloom-user:" + strings.ToLower(name)))
	return hex.EncodeToString(sum[:16])
}

func newToken() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// setting returns a persisted setting, initializing it with gen() once.
func (s *Service) setting(key string, gen func() string) (string, error) {
	if _, err := s.db.Exec(`INSERT OR IGNORE INTO settings (key, value) VALUES (?, ?)`, key, gen()); err != nil {
		return "", err
	}
	var v string
	err := s.db.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	return v, err
}

// ServerID is JellyBloom's stable server ID, generated on first start.
func (s *Service) ServerID() string { return s.serverID }

// User returns a user by ID, or nil.
func (s *Service) User(id string) *User { return s.users[strings.ToLower(id)] }

// Users returns all users.
func (s *Service) Users() []*User {
	out := make([]*User, 0, len(s.users))
	for _, u := range s.users {
		out = append(out, u)
	}
	return out
}

// Authenticate checks a name and password and opens a session for the client.
func (s *Service) Authenticate(ctx context.Context, name, password string, ci ClientInfo) (*Session, *User, error) {
	u := s.byName[strings.ToLower(name)]
	if u == nil {
		bcrypt.CompareHashAndPassword(s.dummy, []byte(password))
		return nil, nil, ErrUnauthorized
	}
	if bcrypt.CompareHashAndPassword(u.hash, []byte(password)) != nil {
		return nil, nil, ErrUnauthorized
	}
	now := time.Now()
	sess := &Session{
		Token: newToken(), UserID: u.ID, DeviceID: ci.DeviceID, DeviceName: ci.Device,
		Client: ci.Client, Version: ci.Version, Created: now, LastSeen: now,
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback()
	// Like Jellyfin, a device holds one session per user: logging in again replaces it.
	if sess.DeviceID != "" {
		if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ? AND device_id = ?`, u.ID, sess.DeviceID); err != nil {
			return nil, nil, err
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO sessions VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		sess.Token, u.ID, sess.DeviceID, sess.DeviceName, sess.Client, sess.Version, now.UnixMilli(), now.UnixMilli())
	if err != nil {
		return nil, nil, err
	}
	return sess, u, tx.Commit()
}

// Validate resolves a token to its session and user.
func (s *Service) Validate(ctx context.Context, token string) (*Session, *User, error) {
	if token == "" {
		return nil, nil, ErrUnauthorized
	}
	sess := &Session{Token: token}
	var created, seen int64
	err := s.db.QueryRowContext(ctx, `SELECT user_id, device_id, device_name, client, version, created, last_seen FROM sessions WHERE token = ?`,
		token).Scan(&sess.UserID, &sess.DeviceID, &sess.DeviceName, &sess.Client, &sess.Version, &created, &seen)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, ErrUnauthorized
	}
	if err != nil {
		return nil, nil, err
	}
	u := s.users[sess.UserID]
	if u == nil { // user removed from config
		return nil, nil, ErrUnauthorized
	}
	sess.Created, sess.LastSeen = time.UnixMilli(created), time.UnixMilli(seen)
	s.touch(ctx, token)
	return sess, u, nil
}

// touch persists last_seen at most once a minute per token.
func (s *Service) touch(ctx context.Context, token string) {
	now := time.Now()
	s.mu.Lock()
	if now.Sub(s.lastSeen[token]) < time.Minute {
		s.mu.Unlock()
		return
	}
	s.lastSeen[token] = now
	s.mu.Unlock()
	s.db.ExecContext(ctx, `UPDATE sessions SET last_seen = ? WHERE token = ?`, now.UnixMilli(), token)
}

// Logout ends a session.
func (s *Service) Logout(ctx context.Context, token string) error {
	s.mu.Lock()
	delete(s.lastSeen, token)
	s.mu.Unlock()
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE token = ?`, token)
	return err
}

// HashPassword returns a bcrypt hash for config.
func HashPassword(password string) (string, error) {
	h, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	return string(h), err
}
