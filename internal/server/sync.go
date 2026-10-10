package server

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/NebulousOinkler/JellyBloom/internal/userdata"
)

// maxSnapshot bounds a merged snapshot (a few hundred thousand records).
const maxSnapshot = 256 << 20

// syncRoutes lets two JellyBloom instances (say, one at home and one on a
// laptop that travels) share user state: GET exports every user's watch
// state and playlists, POST merges another instance's export (the most
// recent change wins per item and playlist). `jellybloom sync` does both
// directions. Admin users only: the export holds every user's state.
func (s *Server) syncRoutes(rt *router) {
	admin := func(h func(http.ResponseWriter, *http.Request, *caller)) http.HandlerFunc {
		return s.authed(func(w http.ResponseWriter, r *http.Request, c *caller) {
			if !c.user.Admin {
				http.Error(w, "Forbidden", http.StatusForbidden)
				return
			}
			if s.opts.Resolver == nil {
				http.NotFound(w, r)
				return
			}
			h(w, r, c)
		})
	}
	rt.handle(http.MethodGet, "/JellyBloom/UserData", admin(func(w http.ResponseWriter, r *http.Request, c *caller) {
		snap, err := s.ud.Export(r.Context(), s.opts.Resolver.KeysOf)
		if err != nil {
			s.internal(w, err)
			return
		}
		writeJSON(w, snap)
	}))
	rt.handle(http.MethodPost, "/JellyBloom/UserData", admin(func(w http.ResponseWriter, r *http.Request, c *caller) {
		var snap userdata.Snapshot
		if err := json.NewDecoder(io.LimitReader(r.Body, maxSnapshot)).Decode(&snap); err != nil {
			http.Error(w, "bad snapshot: "+err.Error(), http.StatusBadRequest)
			return
		}
		st, err := s.ud.Merge(r.Context(), &snap, s.opts.Resolver.Resolve)
		if err != nil {
			s.internal(w, err)
			return
		}
		s.log.Info("merged user state from another instance", "user", c.user.Name, "records", st.Records,
			"playlists", st.Playlists, "deleted playlists", st.DeletedPlaylists)
		writeJSON(w, st)
	}))
}
