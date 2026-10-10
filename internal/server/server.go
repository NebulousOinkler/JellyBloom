// Package server is JellyBloom's client-facing Jellyfin API. It answers what
// JellyBloom owns (server identity, users, sessions, preferences, the socket)
// and hands everything else to the proxy.
package server

import (
	"context"
	"crypto/sha1"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/NebulousOinkler/JellyBloom/internal/auth"
	"github.com/NebulousOinkler/JellyBloom/internal/catalog"
	"github.com/NebulousOinkler/JellyBloom/internal/playback"
	"github.com/NebulousOinkler/JellyBloom/internal/proxy"
	"github.com/NebulousOinkler/JellyBloom/internal/userdata"
)

const schema = `
CREATE TABLE IF NOT EXISTS user_config (
	user_id TEXT PRIMARY KEY,
	config  BLOB NOT NULL
);
CREATE TABLE IF NOT EXISTS display_prefs (
	user_id  TEXT NOT NULL,
	prefs_id TEXT NOT NULL,
	client   TEXT NOT NULL,
	prefs    BLOB NOT NULL,
	PRIMARY KEY (user_id, prefs_id, client)
);
`

// defaultVersion is reported when the web upstream's version is unknown.
const defaultVersion = "10.10.7"

// Options configures a Server.
type Options struct {
	ServerName string
	DB         *sql.DB
	Auth       *auth.Service
	Proxy      *proxy.Proxy
	Catalog    *catalog.Catalog
	Playback   *playback.Handler
	UserData   *userdata.Store
	// Version reports the web upstream's Jellyfin version. JellyBloom claims
	// the same version so the jellyfin-web it serves sees a matching server.
	Version func(ctx context.Context) (string, error)
	Logger  *slog.Logger
}

// Server is the client-facing HTTP handler.
type Server struct {
	opts    Options
	db      *sql.DB
	auth    *auth.Service
	proxy   *proxy.Proxy
	catalog *catalog.Catalog
	ud      *userdata.Store
	hub     *hub
	log     *slog.Logger
	router  *router

	mu        sync.Mutex
	version   string
	versionAt time.Time
}

// New creates the server and its tables.
func New(o Options) (*Server, error) {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if _, err := o.DB.Exec(schema); err != nil {
		return nil, err
	}
	s := &Server{opts: o, db: o.DB, auth: o.Auth, proxy: o.Proxy, catalog: o.Catalog, ud: o.UserData, hub: newHub(o.Logger), log: o.Logger}
	s.routes()
	return s, nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.router.ServeHTTP(w, r) }

// Broadcast sends a message to every connected client socket.
func (s *Server) Broadcast(messageType string, data any) { s.hub.broadcast(messageType, data) }

func (s *Server) routes() {
	rt := &router{fallback: http.HandlerFunc(s.fallback)}
	s.router = rt

	rt.handle(http.MethodGet, "/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "web/", http.StatusFound)
	})
	// The router ignores trailing slashes, so "/web" and "/web/" both land
	// here: only the bare form redirects (redirecting "/web/" would loop).
	rt.handle("", "/web/{rest...}", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/") && r.PathValue("rest") == "" {
			http.Redirect(w, r, "/web/", http.StatusFound)
			return
		}
		s.proxy.Web(w, r)
	})

	rt.handle(http.MethodGet, "/System/Info/Public", s.publicInfo)
	rt.handle(http.MethodGet, "/System/Info", s.authed(s.systemInfo))
	rt.handle(http.MethodGet, "/System/Ping", s.ping)
	rt.handle(http.MethodPost, "/System/Ping", s.ping)
	rt.handle(http.MethodGet, "/System/Endpoint", s.authed(func(w http.ResponseWriter, r *http.Request, c *caller) {
		writeJSON(w, map[string]bool{"IsLocal": false, "IsInNetwork": false})
	}))
	rt.handle(http.MethodGet, "/Branding/Configuration", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"LoginDisclaimer": "", "CustomCss": "", "SplashscreenEnabled": false})
	})
	rt.handle(http.MethodGet, "/Branding/Css", css)
	rt.handle(http.MethodGet, "/Branding/Css.css", css)
	rt.handle(http.MethodGet, "/QuickConnect/Enabled", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, false) })

	rt.handle(http.MethodPost, "/Users/AuthenticateByName", s.authenticate)
	rt.handle(http.MethodGet, "/Users/Public", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, []any{}) })
	rt.handle(http.MethodGet, "/Users", s.authed(func(w http.ResponseWriter, r *http.Request, c *caller) {
		writeJSON(w, []any{s.userDto(r.Context(), c.user)})
	}))
	rt.handle(http.MethodGet, "/Users/Me", s.authed(func(w http.ResponseWriter, r *http.Request, c *caller) {
		writeJSON(w, s.userDto(r.Context(), c.user))
	}))
	rt.handle(http.MethodGet, "/Users/{userId}", s.authed(s.getUser))
	rt.handle(http.MethodPost, "/Users/Configuration", s.authed(s.saveUserConfig))
	rt.handle(http.MethodPost, "/Users/{userId}/Configuration", s.authed(s.saveUserConfig))
	rt.handle("", "/Users/{userId}/Images/{rest...}", func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r) // JellyBloom users have no avatars
	})
	rt.handle("", "/UserImage", func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })

	rt.handle(http.MethodGet, "/Sessions", s.authed(func(w http.ResponseWriter, r *http.Request, c *caller) {
		writeJSON(w, []any{s.sessionDto(c)})
	}))
	rt.handle(http.MethodPost, "/Sessions/Logout", s.authed(func(w http.ResponseWriter, r *http.Request, c *caller) {
		if err := s.auth.Logout(r.Context(), c.sess.Token); err != nil {
			s.internal(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	noContent := s.authed(func(w http.ResponseWriter, r *http.Request, c *caller) { w.WriteHeader(http.StatusNoContent) })
	rt.handle(http.MethodPost, "/Sessions/Capabilities", noContent)
	rt.handle(http.MethodPost, "/Sessions/Capabilities/Full", noContent)

	rt.handle(http.MethodGet, "/DisplayPreferences/{id}", s.authed(s.getDisplayPrefs))
	rt.handle(http.MethodPost, "/DisplayPreferences/{id}", s.authed(s.saveDisplayPrefs))

	rt.handle(http.MethodGet, "/socket", s.authed(func(w http.ResponseWriter, r *http.Request, c *caller) {
		s.hub.serve(w, r, c.user.ID)
	}))

	s.userStateRoutes(rt)
	s.listRoutes(rt)
	s.browseRoutes(rt)

	playbackInfo := s.authed(func(w http.ResponseWriter, r *http.Request, c *caller) {
		s.opts.Playback.PlaybackInfo(w, r, proxyCaller(c))
	})
	rt.handle(http.MethodGet, "/Items/{id}/PlaybackInfo", playbackInfo)
	rt.handle(http.MethodPost, "/Items/{id}/PlaybackInfo", playbackInfo)
}

// caller is an authenticated request's session.
type caller struct {
	sess *auth.Session
	user *auth.User
	info auth.ClientInfo
}

func (s *Server) identify(r *http.Request) (*caller, error) {
	ci := auth.ParseRequest(r)
	sess, user, err := s.auth.Validate(r.Context(), ci.Token)
	if err != nil {
		return nil, err
	}
	return &caller{sess: sess, user: user, info: ci}, nil
}

func (s *Server) authed(h func(http.ResponseWriter, *http.Request, *caller)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := s.identify(r)
		if errors.Is(err, auth.ErrUnauthorized) {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		if err != nil {
			s.internal(w, err)
			return
		}
		h(w, r, c)
	}
}

// fallback applies the forwarding policy, then proxies.
func (s *Server) fallback(w http.ResponseWriter, r *http.Request) {
	switch s.proxy.Decide(r) {
	case proxy.Deny:
		http.Error(w, "Forbidden", http.StatusForbidden)
	case proxy.Anonymous:
		var pc proxy.Caller
		if c, err := s.identify(r); err == nil {
			pc = proxyCaller(c)
		}
		s.proxy.Fallback(w, r, pc)
	default:
		s.authed(func(w http.ResponseWriter, r *http.Request, c *caller) {
			s.proxy.Fallback(w, r, proxyCaller(c))
		})(w, r)
	}
}

func proxyCaller(c *caller) proxy.Caller {
	pc := proxy.Caller{UserID: c.user.ID, Token: c.sess.Token, Client: c.sess.Client, Device: c.sess.DeviceName,
		DeviceID: c.sess.DeviceID, Version: c.sess.Version}
	// What the request itself says wins: it reflects the client making this
	// call. Stream URLs often carry only a DeviceId query parameter.
	for _, f := range []struct {
		dst *string
		v   string
	}{
		{&pc.DeviceID, c.info.DeviceID}, {&pc.Device, c.info.Device}, {&pc.Client, c.info.Client}, {&pc.Version, c.info.Version},
	} {
		if f.v != "" {
			*f.dst = f.v
		}
	}
	return pc
}

func (s *Server) internal(w http.ResponseWriter, err error) {
	s.log.Error("request failed", "err", err)
	http.Error(w, "Internal Server Error", http.StatusInternalServerError)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(v)
}

func css(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/css")
}

func (s *Server) ping(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, "Jellyfin Server")
}

// serverVersion returns the web upstream's version, cached for 10 minutes.
func (s *Server) serverVersion(ctx context.Context) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.version != "" && time.Since(s.versionAt) < 10*time.Minute {
		return s.version
	}
	if s.opts.Version != nil {
		vctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		v, err := s.opts.Version(vctx)
		cancel()
		if err == nil && v != "" {
			s.version, s.versionAt = v, time.Now()
			return v
		}
	}
	if s.version == "" {
		return defaultVersion
	}
	return s.version
}

func localAddress(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

func (s *Server) publicInfoMap(r *http.Request) map[string]any {
	return map[string]any{
		"LocalAddress":           localAddress(r),
		"ServerName":             s.opts.ServerName,
		"Version":                s.serverVersion(r.Context()),
		"ProductName":            "Jellyfin Server",
		"OperatingSystem":        "",
		"Id":                     s.auth.ServerID(),
		"StartupWizardCompleted": true,
	}
}

func (s *Server) publicInfo(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.publicInfoMap(r))
}

func (s *Server) systemInfo(w http.ResponseWriter, r *http.Request, c *caller) {
	info := s.publicInfoMap(r)
	port := 0
	if _, p, err := net.SplitHostPort(r.Host); err == nil {
		port, _ = strconv.Atoi(p)
	}
	for k, v := range map[string]any{
		"OperatingSystemDisplayName": "", "PackageName": "", "HasPendingRestart": false, "IsShuttingDown": false,
		"SupportsLibraryMonitor": true, "WebSocketPortNumber": port, "CompletedInstallations": []any{},
		"CanSelfRestart": false, "CanLaunchWebBrowser": false, "ProgramDataPath": "", "WebPath": "",
		"ItemsByNamePath": "", "CachePath": "", "LogPath": "", "InternalMetadataPath": "",
		"TranscodingTempPath": "", "CastReceiverApplications": []any{}, "HasUpdateAvailable": false,
		"EncoderLocation": "System", "SystemArchitecture": "X64",
	} {
		info[k] = v
	}
	writeJSON(w, info)
}

// authenticate handles POST /Users/AuthenticateByName.
func (s *Server) authenticate(w http.ResponseWriter, r *http.Request) {
	var body struct{ Username, Pw string }
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&body); err != nil {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}
	ci := auth.ParseRequest(r)
	sess, user, err := s.auth.Authenticate(r.Context(), body.Username, body.Pw, ci)
	if errors.Is(err, auth.ErrUnauthorized) {
		s.log.Warn("failed login", "user", body.Username, "client", ci.Client, "remote", r.RemoteAddr)
		http.Error(w, "Invalid username or password entered.", http.StatusUnauthorized)
		return
	}
	if err != nil {
		s.internal(w, err)
		return
	}
	c := &caller{sess: sess, user: user, info: ci}
	writeJSON(w, map[string]any{
		"User":        s.userDto(r.Context(), user),
		"SessionInfo": s.sessionDto(c),
		"AccessToken": sess.Token,
		"ServerId":    s.auth.ServerID(),
	})
}

func (s *Server) getUser(w http.ResponseWriter, r *http.Request, c *caller) {
	if !strings.EqualFold(normalizeHexID(r.PathValue("userId")), c.user.ID) {
		http.NotFound(w, r) // users only see themselves
		return
	}
	writeJSON(w, s.userDto(r.Context(), c.user))
}

func normalizeHexID(id string) string { return strings.ToLower(strings.ReplaceAll(id, "-", "")) }

// userDto builds a Jellyfin UserDto. Policy never claims administrator rights:
// JellyBloom does not expose upstream administration, and jellyfin-web would
// otherwise show a dashboard whose calls are all blocked.
func (s *Server) userDto(ctx context.Context, u *auth.User) map[string]any {
	return map[string]any{
		"Name":                      u.Name,
		"ServerId":                  s.auth.ServerID(),
		"Id":                        u.ID,
		"HasPassword":               true,
		"HasConfiguredPassword":     true,
		"HasConfiguredEasyPassword": false,
		"EnableAutoLogin":           false,
		"Configuration":             s.userConfig(ctx, u.ID),
		"Policy": map[string]any{
			"IsAdministrator": false, "IsHidden": false, "IsDisabled": false,
			"EnableUserPreferenceAccess": true, "EnableRemoteControlOfOtherUsers": false,
			"EnableSharedDeviceControl": false, "EnableRemoteAccess": true,
			"EnableLiveTvManagement": false, "EnableLiveTvAccess": false,
			"EnableMediaPlayback": true, "EnableAudioPlaybackTranscoding": true,
			"EnableVideoPlaybackTranscoding": true, "EnablePlaybackRemuxing": true,
			"ForceRemoteSourceTranscoding": false, "EnableContentDeletion": false,
			"EnableContentDeletionFromFolders": []string{}, "EnableContentDownloading": true,
			"EnableSyncTranscoding": true, "EnableMediaConversion": false,
			"EnabledDevices": []string{}, "EnableAllDevices": true,
			"EnabledChannels": []string{}, "EnableAllChannels": true,
			"EnabledFolders": []string{}, "EnableAllFolders": true,
			"InvalidLoginAttemptCount": 0, "LoginAttemptsBeforeLockout": -1, "MaxActiveSessions": 0,
			"EnablePublicSharing": false, "BlockedMediaFolders": []string{}, "BlockedChannels": []string{},
			"RemoteClientBitrateLimit": 0, "SyncPlayAccess": "None", "AccessSchedules": []any{},
			"BlockUnratedItems": []string{}, "AllowedTags": []string{}, "BlockedTags": []string{},
			"EnableCollectionManagement": false, "EnableSubtitleManagement": false, "EnableLyricManagement": false,
			"AuthenticationProviderId": "Jellyfin.Server.Implementations.Users.DefaultAuthenticationProvider",
			"PasswordResetProviderId":  "Jellyfin.Server.Implementations.Users.DefaultPasswordResetProvider",
		},
	}
}

func defaultUserConfig() map[string]any {
	return map[string]any{
		"AudioLanguagePreference": "", "PlayDefaultAudioTrack": true, "SubtitleLanguagePreference": "",
		"DisplayMissingEpisodes": false, "GroupedFolders": []string{}, "SubtitleMode": "Default",
		"DisplayCollectionsView": false, "EnableLocalPassword": false, "OrderedViews": []string{},
		"LatestItemsExcludes": []string{}, "MyMediaExcludes": []string{}, "HidePlayedInLatest": true,
		"RememberAudioSelections": true, "RememberSubtitleSelections": true,
		"EnableNextEpisodeAutoPlay": true, "CastReceiverId": "",
	}
}

// userConfig is the defaults overlaid with what the user saved.
func (s *Server) userConfig(ctx context.Context, userID string) map[string]any {
	cfg := defaultUserConfig()
	var raw []byte
	if err := s.db.QueryRowContext(ctx, `SELECT config FROM user_config WHERE user_id = ?`, userID).Scan(&raw); err == nil {
		var saved map[string]any
		if json.Unmarshal(raw, &saved) == nil {
			for k, v := range saved {
				cfg[k] = v
			}
		}
	}
	return cfg
}

// readObject reads a JSON object body (up to 1 MB).
func readObject(r *http.Request) (map[string]any, []byte, error) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return nil, nil, err
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		return nil, nil, errors.New("body must be a JSON object")
	}
	return obj, raw, nil
}

func (s *Server) saveUserConfig(w http.ResponseWriter, r *http.Request, c *caller) {
	target := r.PathValue("userId")
	if target == "" {
		target = r.URL.Query().Get("userId")
	}
	if target != "" && normalizeHexID(target) != c.user.ID {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	_, raw, err := readObject(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	_, err = s.db.ExecContext(r.Context(), `INSERT INTO user_config (user_id, config) VALUES (?, ?)
		ON CONFLICT (user_id) DO UPDATE SET config = excluded.config`, c.user.ID, raw)
	if err != nil {
		s.internal(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) getDisplayPrefs(w http.ResponseWriter, r *http.Request, c *caller) {
	id, client := r.PathValue("id"), r.URL.Query().Get("client")
	var raw []byte
	err := s.db.QueryRowContext(r.Context(), `SELECT prefs FROM display_prefs WHERE user_id = ? AND prefs_id = ? AND client = ?`,
		c.user.ID, id, client).Scan(&raw)
	if err == nil {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Write(raw)
		return
	}
	if !errors.Is(err, sql.ErrNoRows) {
		s.internal(w, err)
		return
	}
	writeJSON(w, map[string]any{
		"Id": id, "SortBy": "SortName", "SortOrder": "Ascending", "RememberIndexing": false,
		"RememberSorting": false, "PrimaryImageHeight": 250, "PrimaryImageWidth": 250,
		"CustomPrefs": map[string]string{}, "ScrollDirection": "Horizontal", "ShowBackdrop": true,
		"ShowSidebar": false, "Client": client, "IndexBy": nil, "ViewType": nil,
	})
}

func (s *Server) saveDisplayPrefs(w http.ResponseWriter, r *http.Request, c *caller) {
	id, client := r.PathValue("id"), r.URL.Query().Get("client")
	_, raw, err := readObject(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	_, err = s.db.ExecContext(r.Context(), `INSERT INTO display_prefs (user_id, prefs_id, client, prefs) VALUES (?, ?, ?, ?)
		ON CONFLICT (user_id, prefs_id, client) DO UPDATE SET prefs = excluded.prefs`, c.user.ID, id, client, raw)
	if err != nil {
		s.internal(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// sessionDto builds a Jellyfin SessionInfoDto for the caller's session.
func (s *Server) sessionDto(c *caller) map[string]any {
	sum := sha1.Sum([]byte("session:" + c.sess.Token))
	return map[string]any{
		"Id":                       hex.EncodeToString(sum[:16]),
		"UserId":                   c.user.ID,
		"UserName":                 c.user.Name,
		"Client":                   c.sess.Client,
		"DeviceName":               c.sess.DeviceName,
		"DeviceId":                 c.sess.DeviceID,
		"ApplicationVersion":       c.sess.Version,
		"LastActivityDate":         time.Now().UTC().Format(time.RFC3339Nano),
		"IsActive":                 true,
		"SupportsMediaControl":     false,
		"SupportsRemoteControl":    false,
		"HasCustomDeviceName":      false,
		"ServerId":                 s.auth.ServerID(),
		"AdditionalUsers":          []any{},
		"PlayableMediaTypes":       []string{},
		"NowPlayingQueue":          []any{},
		"NowPlayingQueueFullItems": []any{},
		"PlayState":                map[string]any{"CanSeek": false, "IsPaused": false, "IsMuted": false, "RepeatMode": "RepeatNone", "PlaybackOrder": "Default"},
		"Capabilities":             map[string]any{"PlayableMediaTypes": []string{}, "SupportedCommands": []string{}, "SupportsMediaControl": false, "SupportsPersistentIdentifier": true},
	}
}
