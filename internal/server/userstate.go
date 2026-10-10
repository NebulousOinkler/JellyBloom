package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/NebulousOinkler/JellyBloom/internal/identity"
	"github.com/NebulousOinkler/JellyBloom/internal/userdata"
)

// userStateRoutes registers played/favorite/rating/user-data and playback
// reporting, in both the modern and the legacy /Users/{userId}/… forms.
// JellyBloom is the source of truth for user state; playback reports are
// still forwarded upstream, which is what ends upstream transcodes.
func (s *Server) userStateRoutes(rt *router) {
	reg := func(method, modern, legacy string, h func(http.ResponseWriter, *http.Request, *caller)) {
		wrapped := s.authed(func(w http.ResponseWriter, r *http.Request, c *caller) {
			if ownUser(w, r, c) {
				h(w, r, c)
			}
		})
		if modern != "" {
			rt.handle(method, modern, wrapped)
		}
		rt.handle(method, legacy, wrapped)
	}

	played := func(on bool) func(http.ResponseWriter, *http.Request, *caller) {
		return func(w http.ResponseWriter, r *http.Request, c *caller) {
			id, ok := s.knownItem(w, r)
			if !ok {
				return
			}
			when := time.Now()
			if v := readParams(r).str("datePlayed"); v != "" {
				if t, err := time.Parse(time.RFC3339, v); err == nil {
					when = t
				}
			}
			// A series, season, or album is marked through its playable items.
			s.updateUserData(w, r, c, id, s.catalog.Leaves(id), func(rec *userdata.Record) {
				if on {
					userdata.MarkPlayed(rec, when)
				} else {
					userdata.MarkUnplayed(rec)
				}
			})
		}
	}
	reg(http.MethodPost, "/UserPlayedItems/{itemId}", "/Users/{userId}/PlayedItems/{itemId}", played(true))
	reg(http.MethodDelete, "/UserPlayedItems/{itemId}", "/Users/{userId}/PlayedItems/{itemId}", played(false))

	favorite := func(on bool) func(http.ResponseWriter, *http.Request, *caller) {
		return func(w http.ResponseWriter, r *http.Request, c *caller) {
			if id, ok := s.knownItem(w, r); ok {
				s.updateUserData(w, r, c, id, []string{id}, func(rec *userdata.Record) { rec.IsFavorite = on })
			}
		}
	}
	reg(http.MethodPost, "/UserFavoriteItems/{itemId}", "/Users/{userId}/FavoriteItems/{itemId}", favorite(true))
	reg(http.MethodDelete, "/UserFavoriteItems/{itemId}", "/Users/{userId}/FavoriteItems/{itemId}", favorite(false))

	reg(http.MethodPost, "/UserItems/{itemId}/Rating", "/Users/{userId}/Items/{itemId}/Rating", func(w http.ResponseWriter, r *http.Request, c *caller) {
		if id, ok := s.knownItem(w, r); ok {
			likes := readParams(r).boolPtr("likes")
			s.updateUserData(w, r, c, id, []string{id}, func(rec *userdata.Record) { rec.Likes = likes })
		}
	})
	reg(http.MethodDelete, "/UserItems/{itemId}/Rating", "/Users/{userId}/Items/{itemId}/Rating", func(w http.ResponseWriter, r *http.Request, c *caller) {
		if id, ok := s.knownItem(w, r); ok {
			s.updateUserData(w, r, c, id, []string{id}, func(rec *userdata.Record) { rec.Likes = nil })
		}
	})

	reg(http.MethodGet, "/UserItems/{itemId}/UserData", "/Users/{userId}/Items/{itemId}/UserData", func(w http.ResponseWriter, r *http.Request, c *caller) {
		if id, ok := s.knownItem(w, r); ok {
			s.writeUserData(w, r, c, id)
		}
	})
	reg(http.MethodPost, "/UserItems/{itemId}/UserData", "/Users/{userId}/Items/{itemId}/UserData", func(w http.ResponseWriter, r *http.Request, c *caller) {
		id, ok := s.knownItem(w, r)
		if !ok {
			return
		}
		var u struct {
			Played, IsFavorite, Likes *bool
			PlaybackPositionTicks     *int64
			PlayCount                 *int
			LastPlayedDate            *time.Time
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&u); err != nil {
			http.Error(w, "bad request body", http.StatusBadRequest)
			return
		}
		s.updateUserData(w, r, c, id, []string{id}, func(rec *userdata.Record) {
			if u.Played != nil {
				rec.Played = *u.Played
			}
			if u.IsFavorite != nil {
				rec.IsFavorite = *u.IsFavorite
			}
			if u.Likes != nil {
				rec.Likes = u.Likes
			}
			if u.PlaybackPositionTicks != nil {
				rec.Position = *u.PlaybackPositionTicks
			}
			if u.PlayCount != nil {
				rec.PlayCount = *u.PlayCount
			}
			if u.LastPlayedDate != nil {
				rec.LastPlayed = *u.LastPlayedDate
			}
		})
	})

	// Playback reports with a JSON body.
	for path, ev := range map[string]userdata.Event{
		"/Sessions/Playing": userdata.Start, "/Sessions/Playing/Progress": userdata.Progress, "/Sessions/Playing/Stopped": userdata.Stop,
	} {
		rt.handle(http.MethodPost, path, s.authed(func(w http.ResponseWriter, r *http.Request, c *caller) { s.playbackReport(w, r, c, ev) }))
	}
	// Legacy reports with query parameters.
	reg(http.MethodPost, "/PlayingItems/{itemId}", "/Users/{userId}/PlayingItems/{itemId}", s.legacyReport(userdata.Start))
	reg(http.MethodPost, "/PlayingItems/{itemId}/Progress", "/Users/{userId}/PlayingItems/{itemId}/Progress", s.legacyReport(userdata.Progress))
	reg(http.MethodDelete, "/PlayingItems/{itemId}", "/Users/{userId}/PlayingItems/{itemId}", s.legacyReport(userdata.Stop))
}

// knownItem reads {itemId} and checks it names a visible item.
func (s *Server) knownItem(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := identity.NormalizeID(r.PathValue("itemId"))
	if !s.catalog.Known(id) {
		http.NotFound(w, r)
		return "", false
	}
	return id, true
}

// updateUserData applies fn to ids, then answers with item's user data and
// tells the user's other clients.
func (s *Server) updateUserData(w http.ResponseWriter, r *http.Request, c *caller, item string, ids []string, fn func(*userdata.Record)) {
	if len(ids) == 0 {
		ids = []string{item}
	}
	if _, err := s.ud.Update(r.Context(), c.user.ID, ids, fn); err != nil {
		s.internal(w, err)
		return
	}
	s.userDataChanged(r, c, append([]string{item}, ids...))
	s.writeUserData(w, r, c, item)
}

func (s *Server) writeUserData(w http.ResponseWriter, r *http.Request, c *caller, id string) {
	dtos, err := s.catalog.UserDataDTOs(r.Context(), c.user.ID, []string{id})
	if err != nil {
		s.internal(w, err)
		return
	}
	writeJSON(w, dtos[id])
}

// userDataChanged pushes a UserDataChanged message to the user's sockets.
func (s *Server) userDataChanged(r *http.Request, c *caller, ids []string) {
	seen := map[string]bool{}
	var unique []string
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			unique = append(unique, id)
		}
	}
	dtos, err := s.catalog.UserDataDTOs(r.Context(), c.user.ID, unique)
	if err != nil {
		return
	}
	list := make([]any, 0, len(unique))
	for _, id := range unique {
		list = append(list, dtos[id])
	}
	s.hub.toUser(c.user.ID, "UserDataChanged", map[string]any{"UserId": c.user.ID, "UserDataList": list})
}

// playbackReport records a /Sessions/Playing* report, then forwards it.
func (s *Server) playbackReport(w http.ResponseWriter, r *http.Request, c *caller, ev userdata.Event) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "bad request body", http.StatusBadRequest)
		return
	}
	var b struct {
		ItemId                                string
		PositionTicks                         *int64
		AudioStreamIndex, SubtitleStreamIndex *int
	}
	if json.Unmarshal(raw, &b) == nil {
		s.recordPlayback(r, c, identity.NormalizeID(b.ItemId), userdata.Report{
			Event: ev, PositionTicks: b.PositionTicks, AudioStream: b.AudioStreamIndex, SubtitleStream: b.SubtitleStreamIndex,
		})
	}
	r.Body = io.NopCloser(bytes.NewReader(raw))
	r.ContentLength = int64(len(raw))
	s.proxy.Fallback(w, r, proxyCaller(c))
}

// legacyReport records a /PlayingItems/{itemId} report, then forwards it.
func (s *Server) legacyReport(ev userdata.Event) func(http.ResponseWriter, *http.Request, *caller) {
	return func(w http.ResponseWriter, r *http.Request, c *caller) {
		p := readParams(r)
		rep := userdata.Report{Event: ev, AudioStream: p.intPtr("audioStreamIndex"), SubtitleStream: p.intPtr("subtitleStreamIndex")}
		if v, err := strconv.ParseInt(p.str("positionTicks"), 10, 64); err == nil {
			rep.PositionTicks = &v
		}
		s.recordPlayback(r, c, identity.NormalizeID(r.PathValue("itemId")), rep)
		s.proxy.Fallback(w, r, proxyCaller(c))
	}
}

func (s *Server) recordPlayback(r *http.Request, c *caller, id string, rep userdata.Report) {
	if !s.catalog.Known(id) {
		return
	}
	runtime := s.catalog.Runtime(id)
	_, err := s.ud.Update(r.Context(), c.user.ID, []string{id}, func(rec *userdata.Record) {
		userdata.Apply(rec, rep, runtime, time.Now())
	})
	if err != nil {
		s.log.Error("recording playback", "err", err)
		return
	}
	if rep.Event != userdata.Progress { // progress would flood clients
		s.userDataChanged(r, c, []string{id})
	}
}
