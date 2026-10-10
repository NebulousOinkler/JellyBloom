package server

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/NebulousOinkler/JellyBloom/internal/catalog"
	"github.com/NebulousOinkler/JellyBloom/internal/identity"
)

// rootFolderID is the user root folder ("Media Folders") whose children are
// the merged libraries.
var rootFolderID = identity.DeriveID("root")

// params reads query parameters case-insensitively, as Jellyfin binds them.
type params map[string][]string

func readParams(r *http.Request) params {
	p := params{}
	for k, vs := range r.URL.Query() {
		lk := strings.ToLower(k)
		p[lk] = append(p[lk], vs...)
	}
	return p
}

func (p params) str(k string) string {
	if vs := p[strings.ToLower(k)]; len(vs) > 0 {
		return vs[0]
	}
	return ""
}

// list splits comma-separated values (and repeated keys); sep adds separators
// such as "|" used by genre names.
func (p params) list(k string, sep ...string) []string {
	var out []string
	for _, v := range p[strings.ToLower(k)] {
		for _, s := range sep {
			v = strings.ReplaceAll(v, s, ",")
		}
		for _, part := range strings.Split(v, ",") {
			if part = strings.TrimSpace(part); part != "" {
				out = append(out, part)
			}
		}
	}
	return out
}

func (p params) boolPtr(k string) *bool {
	v := p.str(k)
	if v == "" {
		return nil
	}
	b := strings.EqualFold(v, "true")
	return &b
}

func (p params) int(k string, def int) int {
	if n, err := strconv.Atoi(p.str(k)); err == nil {
		return n
	}
	return def
}

func (p params) intPtr(k string) *int {
	if n, err := strconv.Atoi(p.str(k)); err == nil {
		return &n
	}
	return nil
}

// time parses an ISO 8601 date (Jellyfin clients send RFC 3339, sometimes
// with 7 fractional digits or no zone); nil if absent or invalid.
func (p params) time(k string) *time.Time {
	v := p.str(k)
	if v == "" {
		return nil
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.9999999", "2006-01-02"} {
		if t, err := time.Parse(layout, v); err == nil {
			return &t
		}
	}
	return nil
}

func (p params) query() catalog.Query {
	q := catalog.Query{
		ParentID:                p.str("parentId"),
		Recursive:               strings.EqualFold(p.str("recursive"), "true"),
		IDs:                     p.list("ids"),
		IncludeTypes:            p.list("includeItemTypes"),
		ExcludeTypes:            p.list("excludeItemTypes"),
		MediaTypes:              p.list("mediaTypes"),
		SearchTerm:              p.str("searchTerm"),
		SortBy:                  p.list("sortBy"),
		SortOrder:               p.list("sortOrder"),
		StartIndex:              p.int("startIndex", 0),
		Limit:                   p.int("limit", 0),
		GenreIDs:                p.list("genreIds", "|"),
		StudioIDs:               p.list("studioIds", "|"),
		PersonIDs:               p.list("personIds", "|"),
		ArtistIDs:               p.list("artistIds", "|"),
		AlbumArtistIDs:          p.list("albumArtistIds", "|"),
		AlbumIDs:                p.list("albumIds", "|"),
		Genres:                  p.list("genres", "|"),
		IsFolder:                p.boolPtr("isFolder"),
		NameStartsWith:          p.str("nameStartsWith"),
		NameStartsWithOrGreater: p.str("nameStartsWithOrGreater"),
		NameLessThan:            p.str("nameLessThan"),
		Filters:                 p.list("filters"),
		SeriesID:                p.str("seriesId"),
		ParentIndexNumber:       p.intPtr("parentIndexNumber"),
		IsFavorite:              p.boolPtr("isFavorite"),
		IsPlayed:                p.boolPtr("isPlayed"),
	}
	for _, y := range p.list("years") {
		if n, err := strconv.Atoi(y); err == nil {
			q.Years = append(q.Years, n)
		}
	}
	if q.ParentID != "" && identity.NormalizeID(q.ParentID) == rootFolderID {
		q.ParentID = ""
	}
	return q
}

// listOptions returns DTO options for list endpoints: only requested
// optional fields, as Jellyfin does.
func (p params) listOptions(c *caller) catalog.DTOOptions {
	fields := p.list("fields")
	if fields == nil {
		fields = []string{}
	}
	return catalog.DTOOptions{Fields: fields, UserID: c.user.ID}
}

// ownUser rejects legacy /Users/{userId}/... routes for other users.
func ownUser(w http.ResponseWriter, r *http.Request, c *caller) bool {
	if id := r.PathValue("userId"); id != "" && normalizeHexID(id) != c.user.ID {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return false
	}
	return true
}

func emptyResult() map[string]any {
	return map[string]any{"Items": []any{}, "TotalRecordCount": 0, "StartIndex": 0}
}

func (s *Server) browseRoutes(rt *router) {
	both := func(method, path string, h func(http.ResponseWriter, *http.Request, *caller)) {
		wrapped := s.authed(func(w http.ResponseWriter, r *http.Request, c *caller) {
			if ownUser(w, r, c) {
				h(w, r, c)
			}
		})
		rt.handle(method, path, wrapped)
		rt.handle(method, "/Users/{userId}"+path, wrapped)
	}

	views := func(w http.ResponseWriter, r *http.Request, c *caller) {
		res, err := s.catalog.Views(r.Context(), readParams(r).listOptions(c))
		if err == nil {
			err = s.addLocalViews(r.Context(), c, res)
		}
		s.writeResult(w, res, err)
	}
	rt.handle(http.MethodGet, "/UserViews", s.authed(views))
	rt.handle(http.MethodGet, "/Users/{userId}/Views", s.authed(func(w http.ResponseWriter, r *http.Request, c *caller) {
		if ownUser(w, r, c) {
			views(w, r, c)
		}
	}))

	// Fixed /Items/... routes must precede /Items/{id}.
	both(http.MethodGet, "/Items/Latest", func(w http.ResponseWriter, r *http.Request, c *caller) {
		p := readParams(r)
		group := !strings.EqualFold(p.str("groupItems"), "false")
		parent := p.str("parentId")
		if identity.NormalizeID(parent) == rootFolderID {
			parent = ""
		}
		hidePlayed := s.userConfig(r.Context(), c.user.ID)["HidePlayedInLatest"] != false
		items, err := s.catalog.Latest(r.Context(), parent, p.list("includeItemTypes"), group, hidePlayed, p.int("limit", 20), p.listOptions(c))
		if err != nil {
			s.internal(w, err)
			return
		}
		writeJSON(w, items)
	})
	both(http.MethodGet, "/Items/Resume", func(w http.ResponseWriter, r *http.Request, c *caller) {
		p := readParams(r)
		q := p.query()
		if q.Limit == 0 {
			q.Limit = 12
		}
		res, err := s.catalog.Resume(r.Context(), q, p.listOptions(c))
		s.writeResult(w, res, err)
	})
	rt.handle(http.MethodGet, "/Shows/NextUp", s.authed(func(w http.ResponseWriter, r *http.Request, c *caller) {
		p := readParams(r)
		parent := p.str("parentId")
		if identity.NormalizeID(parent) == rootFolderID {
			parent = ""
		}
		n := catalog.NextUpOptions{
			SeriesID: p.str("seriesId"), ParentID: parent, StartIndex: p.int("startIndex", 0), Limit: p.int("limit", 0),
			SkipResumable: strings.EqualFold(p.str("enableResumable"), "false"),
			Rewatching:    strings.EqualFold(p.str("enableRewatching"), "true"),
		}
		if t := p.time("nextUpDateCutoff"); t != nil {
			n.Cutoff = *t
		}
		// disableFirstEpisode is accepted and ignored, as Jellyfin 12.2 does.
		res, err := s.catalog.NextUp(r.Context(), n, p.listOptions(c))
		s.writeResult(w, res, err)
	}))
	rt.handle(http.MethodGet, "/Shows/Upcoming", s.authed(func(w http.ResponseWriter, r *http.Request, c *caller) {
		writeJSON(w, emptyResult()) // no air dates for future episodes in upstream libraries
	}))
	both(http.MethodGet, "/Items/Filters", func(w http.ResponseWriter, r *http.Request, c *caller) {
		q := readParams(r).query()
		q.Recursive = true
		writeJSON(w, s.catalog.Filters(q))
	})
	both(http.MethodGet, "/Items/Filters2", func(w http.ResponseWriter, r *http.Request, c *caller) {
		q := readParams(r).query()
		q.Recursive = true
		writeJSON(w, s.catalog.Filters2(q))
	})
	both(http.MethodGet, "/Items/Root", func(w http.ResponseWriter, r *http.Request, c *caller) {
		writeJSON(w, s.rootFolder())
	})
	both(http.MethodGet, "/Items", s.items)
	both(http.MethodGet, "/Items/{id}", func(w http.ResponseWriter, r *http.Request, c *caller) {
		id := r.PathValue("id")
		if identity.NormalizeID(id) == rootFolderID {
			writeJSON(w, s.rootFolder())
			return
		}
		if local := s.localItem(r.Context(), c, identity.NormalizeID(id)); local != nil {
			writeJSON(w, local)
			return
		}
		item, err := s.catalog.Item(r.Context(), id, catalog.DTOOptions{UserID: c.user.ID})
		s.writeItem(w, r, item, err)
	})

	rt.handle(http.MethodGet, "/Shows/{id}/Seasons", s.authed(func(w http.ResponseWriter, r *http.Request, c *caller) {
		res, err := s.catalog.Seasons(r.Context(), r.PathValue("id"), readParams(r).listOptions(c))
		if err != nil {
			s.internal(w, err)
			return
		}
		writeJSON(w, res)
	}))
	rt.handle(http.MethodGet, "/Shows/{id}/Episodes", s.authed(func(w http.ResponseWriter, r *http.Request, c *caller) {
		p := readParams(r)
		res, err := s.catalog.Episodes(r.Context(), r.PathValue("id"), p.str("seasonId"), p.intPtr("season"),
			p.int("startIndex", 0), p.int("limit", 0), p.listOptions(c))
		if err != nil {
			s.internal(w, err)
			return
		}
		writeJSON(w, res)
	}))

	rt.handle(http.MethodGet, "/Search/Hints", s.authed(func(w http.ResponseWriter, r *http.Request, c *caller) {
		p := readParams(r)
		q := p.query()
		q.Recursive = true
		if q.Limit == 0 {
			q.Limit = 20
		}
		res, err := s.catalog.SearchHints(r.Context(), q, catalog.DTOOptions{UserID: c.user.ID})
		if err != nil {
			s.internal(w, err)
			return
		}
		writeJSON(w, res)
	}))

	artists := func(albumArtists bool) http.HandlerFunc {
		return s.authed(func(w http.ResponseWriter, r *http.Request, c *caller) {
			p := readParams(r)
			q := p.query()
			q.Recursive = true
			res, err := s.catalog.Artists(r.Context(), q, albumArtists, p.listOptions(c))
			if err != nil {
				s.internal(w, err)
				return
			}
			writeJSON(w, res)
		})
	}
	rt.handle(http.MethodGet, "/Artists", artists(false))
	rt.handle(http.MethodGet, "/Artists/AlbumArtists", artists(true))
	rt.handle(http.MethodGet, "/Artists/{name}", s.authed(func(w http.ResponseWriter, r *http.Request, c *caller) {
		item, err := s.catalog.ArtistByName(r.Context(), r.PathValue("name"), catalog.DTOOptions{UserID: c.user.ID})
		s.writeItem(w, r, item, err)
	}))

	for path, kind := range map[string]string{"/Genres": "Genre", "/MusicGenres": "MusicGenre", "/Studios": "Studio", "/Persons": "Person"} {
		rt.handle(http.MethodGet, path, s.authed(func(w http.ResponseWriter, r *http.Request, c *caller) {
			p := readParams(r)
			q := p.query()
			q.Recursive = true
			kinds := []string{kind}
			if kind == "Genre" && q.ParentID == "" {
				kinds = append(kinds, "MusicGenre") // Jellyfin's /Genres without a library spans both
			}
			res, err := s.catalog.Entities(r.Context(), q, p.listOptions(c), kinds...)
			if err != nil {
				s.internal(w, err)
				return
			}
			writeJSON(w, res)
		}))
		rt.handle(http.MethodGet, path+"/{name}", s.authed(func(w http.ResponseWriter, r *http.Request, c *caller) {
			name, err := url.PathUnescape(r.PathValue("name"))
			if err != nil {
				name = r.PathValue("name")
			}
			item, err := s.catalog.EntityByName(r.Context(), kind, name, catalog.DTOOptions{UserID: c.user.ID})
			s.writeItem(w, r, item, err)
		}))
	}
}

func (s *Server) writeResult(w http.ResponseWriter, res *catalog.QueryResult, err error) {
	if err != nil {
		s.internal(w, err)
		return
	}
	writeJSON(w, res)
}

func (s *Server) writeItem(w http.ResponseWriter, r *http.Request, item map[string]any, err error) {
	if err != nil {
		s.internal(w, err)
		return
	}
	if item == nil {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, item)
}

// items answers /Items. With no parent, recursion, IDs, or search, Jellyfin
// returns the user root's children: the libraries.
func (s *Server) items(w http.ResponseWriter, r *http.Request, c *caller) {
	p := readParams(r)
	q := p.query()
	if s.localItems(w, r, c, q, p) {
		return
	}
	if q.ParentID == "" && !q.Recursive && len(q.IDs) == 0 && q.SearchTerm == "" {
		res, err := s.catalog.Views(r.Context(), p.listOptions(c))
		if err == nil {
			err = s.addLocalViews(r.Context(), c, res)
		}
		s.writeResult(w, res, err)
		return
	}
	res, err := s.catalog.Query(r.Context(), q, p.listOptions(c))
	if err != nil {
		s.internal(w, err)
		return
	}
	writeJSON(w, res)
}

func (s *Server) rootFolder() map[string]any {
	return map[string]any{
		"Name": "Media Folders", "ServerId": s.auth.ServerID(), "Id": rootFolderID, "Type": "UserRootFolder",
		"IsFolder": true, "CollectionType": nil, "ImageTags": map[string]any{}, "BackdropImageTags": []any{},
		"LocationType": "FileSystem", "MediaType": "Unknown",
		"UserData": map[string]any{"PlaybackPositionTicks": 0, "PlayCount": 0, "IsFavorite": false, "Played": false, "Key": rootFolderID},
	}
}
