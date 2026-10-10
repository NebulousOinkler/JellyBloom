package sources

import (
	"slices"
	"time"
)

// sessionIdle is how long a play session is kept after its last use.
const sessionIdle = 12 * time.Hour

// Session is a play session: what PlaybackInfo offered, and the upstream
// now serving it. Streams, HLS, subtitles, and progress reports all follow
// its active upstream, so a failover moves all of them together.
type Session struct {
	ID       string              // JellyBloom play session ID
	User     string              // JellyBloom user ID
	Device   string              // client device ID
	Item     string              // unified item ID
	Versions map[string][]Source // version (media source) ID -> sources, best first

	active  string // upstream serving the session ("" until the first stream)
	touched time.Time
}

// StartSession records a play session from PlaybackInfo.
func (r *Registry) StartSession(s Session) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s.touched = r.now()
	r.sessions[s.ID] = &s
}

// find returns the session named by id, else the newest session of the
// user's device for the item (stream URLs may omit the play session).
// Callers hold r.mu.
func (r *Registry) find(id, user, device, item string) *Session {
	if s := r.sessions[id]; s != nil {
		return s
	}
	if user == "" || item == "" {
		return nil
	}
	var best *Session
	for _, s := range r.sessions {
		if s.User == user && s.Device == device && s.Item == item && (best == nil || s.touched.After(best.touched)) {
			best = s
		}
	}
	return best
}

// Active returns the upstream serving a play session ("" if none yet).
func (r *Registry) Active(id, user, device, item string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s := r.find(id, user, device, item); s != nil {
		s.touched = r.now()
		return s.active
	}
	return ""
}

// SetActive records the upstream now serving a play session.
func (r *Registry) SetActive(id, user, device, item, upstream string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s := r.find(id, user, device, item); s != nil && s.active != upstream {
		if s.active != "" {
			r.log.Info("play session moved", "session", s.ID, "from", s.active, "to", upstream)
		}
		s.active, s.touched = upstream, r.now()
	}
}

// Prefer returns the upstream a request should go to: the active upstream
// of the first play session the request names (by play session ID, or by
// item for the caller's device), else "".
func (r *Registry) Prefer(user, device string, ids []string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, id := range ids {
		if s := r.sessions[id]; s != nil && s.active != "" {
			s.touched = r.now()
			return s.active
		}
	}
	for _, id := range ids {
		if s := r.find("", user, device, id); s != nil && s.active != "" {
			s.touched = r.now()
			return s.active
		}
	}
	return ""
}

// Moved records that a request about ids failed on upstream and was served
// by next: sessions it names move to next.
func (r *Registry) Moved(user, device string, ids []string, from, next string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, id := range ids {
		s := r.sessions[id]
		if s == nil {
			s = r.find("", user, device, id)
		}
		if s != nil && s.active != next && (s.active == from || s.active == "") {
			r.log.Info("play session moved", "session", s.ID, "from", from, "to", next)
			s.active, s.touched = next, r.now()
		}
	}
}

// expireSessions drops idle sessions.
func (r *Registry) expireSessions() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, s := range r.sessions {
		if r.now().Sub(s.touched) > sessionIdle {
			delete(r.sessions, id)
		}
	}
}

// VersionSources returns the sources PlaybackInfo offered for a version in
// a play session (found as Active finds it), or nil.
func (r *Registry) VersionSources(id, user, device, item, version string) []Source {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s := r.find(id, user, device, item); s != nil {
		return slices.Clone(s.Versions[version])
	}
	return nil
}
