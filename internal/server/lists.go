package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/NebulousOinkler/JellyBloom/internal/catalog"
	"github.com/NebulousOinkler/JellyBloom/internal/identity"
	"github.com/NebulousOinkler/JellyBloom/internal/userdata"
)

// Playlists (private, ordered) and collections (shared) are JellyBloom's own;
// they hold unified item IDs and appear in the same Playlists / Collections
// libraries an upstream would show (same derived IDs as the catalog's).
var (
	playlistsViewID   = identity.DeriveID("view:playlists:playlists")
	collectionsViewID = identity.DeriveID("view:boxsets:collections")
)

func (s *Server) listRoutes(rt *router) {
	a := func(h func(http.ResponseWriter, *http.Request, *caller)) http.HandlerFunc { return s.authed(h) }

	rt.handle(http.MethodPost, "/Playlists", a(s.createPlaylist))
	rt.handle(http.MethodGet, "/Playlists/{id}", a(func(w http.ResponseWriter, r *http.Request, c *caller) {
		if p := s.playlist(w, r, c); p != nil {
			writeJSON(w, map[string]any{"OpenAccess": false, "Shares": []any{}, "ItemIds": p.ItemIDs()})
		}
	}))
	rt.handle(http.MethodPost, "/Playlists/{id}", a(func(w http.ResponseWriter, r *http.Request, c *caller) {
		var b struct {
			Name string
			Ids  []string
		}
		json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&b)
		var items []string
		if b.Ids != nil {
			items = s.knownIDs(b.Ids)
		}
		s.listResult(w, r, s.ud.UpdatePlaylist(r.Context(), c.user.ID, r.PathValue("id"), b.Name, items))
	}))
	rt.handle(http.MethodGet, "/Playlists/{id}/Items", a(func(w http.ResponseWriter, r *http.Request, c *caller) {
		if p := s.playlist(w, r, c); p != nil {
			s.writePlaylistItems(w, r, c, p)
		}
	}))
	rt.handle(http.MethodPost, "/Playlists/{id}/Items", a(func(w http.ResponseWriter, r *http.Request, c *caller) {
		s.listResult(w, r, s.ud.AddToPlaylist(r.Context(), c.user.ID, r.PathValue("id"), s.knownIDs(readParams(r).list("ids"))))
	}))
	rt.handle(http.MethodDelete, "/Playlists/{id}/Items", a(func(w http.ResponseWriter, r *http.Request, c *caller) {
		entries := readParams(r).list("entryIds")
		s.listResult(w, r, s.ud.RemoveFromPlaylist(r.Context(), c.user.ID, r.PathValue("id"), normalizeAll(entries)))
	}))
	rt.handle(http.MethodPost, "/Playlists/{id}/Items/{entryId}/Move/{index}", a(func(w http.ResponseWriter, r *http.Request, c *caller) {
		idx, err := parseIndex(r.PathValue("index"))
		if err != nil {
			http.Error(w, "bad index", http.StatusBadRequest)
			return
		}
		s.listResult(w, r, s.ud.MovePlaylistEntry(r.Context(), c.user.ID, r.PathValue("id"), identity.NormalizeID(r.PathValue("entryId")), idx))
	}))

	rt.handle(http.MethodPost, "/Collections", a(func(w http.ResponseWriter, r *http.Request, c *caller) {
		p := readParams(r)
		name := strings.TrimSpace(p.str("name"))
		if name == "" {
			http.Error(w, "name is required", http.StatusBadRequest)
			return
		}
		col, err := s.ud.CreateCollection(r.Context(), name, s.knownIDs(p.list("ids")))
		if err != nil {
			s.internal(w, err)
			return
		}
		writeJSON(w, map[string]string{"Id": col.ID})
	}))
	rt.handle(http.MethodPost, "/Collections/{id}/Items", a(func(w http.ResponseWriter, r *http.Request, c *caller) {
		if _, err := s.ud.Collection(r.Context(), r.PathValue("id")); err != nil {
			s.listResult(w, r, err)
			return
		}
		s.listResult(w, r, s.ud.AddToCollection(r.Context(), r.PathValue("id"), s.knownIDs(readParams(r).list("ids"))))
	}))
	rt.handle(http.MethodDelete, "/Collections/{id}/Items", a(func(w http.ResponseWriter, r *http.Request, c *caller) {
		s.listResult(w, r, s.ud.RemoveFromCollection(r.Context(), r.PathValue("id"), normalizeAll(readParams(r).list("ids"))))
	}))

	// Deleting items is otherwise blocked (it would delete upstream media);
	// JellyBloom's own playlists and collections can be deleted.
	rt.handle(http.MethodDelete, "/Items/{id}", a(func(w http.ResponseWriter, r *http.Request, c *caller) {
		ctx, id := r.Context(), identity.NormalizeID(r.PathValue("id"))
		err := s.ud.DeletePlaylist(ctx, c.user.ID, id)
		if errors.Is(err, userdata.ErrNotFound) {
			err = s.ud.DeleteCollection(ctx, id)
		}
		if errors.Is(err, userdata.ErrNotFound) {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		s.listResult(w, r, err)
	}))
}

func normalizeAll(ids []string) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = identity.NormalizeID(id)
	}
	return out
}

func parseIndex(s string) (int, error) {
	var n int
	err := json.Unmarshal([]byte(s), &n)
	return n, err
}

// knownIDs keeps visible item IDs (normalized), in order.
func (s *Server) knownIDs(ids []string) []string {
	var out []string
	for _, id := range normalizeAll(ids) {
		if s.catalog.Known(id) {
			out = append(out, id)
		}
	}
	return out
}

func (s *Server) listResult(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, userdata.ErrNotFound):
		http.NotFound(w, r)
	case err != nil:
		s.internal(w, err)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *Server) playlist(w http.ResponseWriter, r *http.Request, c *caller) *userdata.Playlist {
	p, err := s.ud.Playlist(r.Context(), c.user.ID, identity.NormalizeID(r.PathValue("id")))
	if err != nil {
		s.listResult(w, r, err)
		return nil
	}
	return p
}

func (s *Server) createPlaylist(w http.ResponseWriter, r *http.Request, c *caller) {
	p := readParams(r)
	var b struct {
		Name, MediaType, UserId string
		Ids                     []string
	}
	if r.ContentLength != 0 {
		json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&b)
	}
	if b.Name == "" {
		b.Name = p.str("name")
	}
	if b.MediaType == "" {
		b.MediaType = p.str("mediaType")
	}
	if b.UserId == "" {
		b.UserId = p.str("userId")
	}
	if b.UserId != "" && normalizeHexID(b.UserId) != c.user.ID {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	if len(b.Ids) == 0 {
		b.Ids = p.list("ids")
	}
	b.Name = strings.TrimSpace(b.Name)
	if b.Name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}
	items := s.knownIDs(b.Ids)
	if b.MediaType == "" {
		b.MediaType = "Video"
		if len(items) > 0 && s.catalog.MediaType(items[0]) == "Audio" {
			b.MediaType = "Audio"
		}
	}
	pl, err := s.ud.CreatePlaylist(r.Context(), c.user.ID, b.Name, b.MediaType, items)
	if err != nil {
		s.internal(w, err)
		return
	}
	writeJSON(w, map[string]string{"Id": pl.ID})
}

func folderDTO(id, name, typ, parent string, children int, created time.Time, serverID string) map[string]any {
	return map[string]any{
		"Name": name, "SortName": strings.ToLower(name), "ServerId": serverID, "Id": id, "Type": typ,
		"IsFolder": true, "ParentId": parent, "ChildCount": children, "RecursiveItemCount": children,
		"DateCreated": created.UTC().Format(time.RFC3339Nano), "CanDelete": true, "LocationType": "FileSystem",
		"ImageTags": map[string]any{}, "BackdropImageTags": []any{}, "ImageBlurHashes": map[string]any{},
		"PlayAccess": "Full", "UserData": map[string]any{"PlaybackPositionTicks": 0, "PlayCount": 0,
			"IsFavorite": false, "Played": false, "Key": id, "ItemId": id},
	}
}

func (s *Server) playlistDTO(p *userdata.Playlist) map[string]any {
	d := folderDTO(p.ID, p.Name, "Playlist", playlistsViewID, len(p.Entries), p.Created, s.auth.ServerID())
	d["MediaType"] = p.MediaType
	var runtime int64
	for _, e := range p.Entries {
		runtime += s.catalog.Runtime(e.ItemID)
	}
	d["CumulativeRunTimeTicks"], d["RunTimeTicks"] = runtime, runtime
	return d
}

func (s *Server) collectionDTO(col *userdata.Collection) map[string]any {
	return folderDTO(col.ID, col.Name, "BoxSet", collectionsViewID, len(col.Items), col.Created, s.auth.ServerID())
}

func (s *Server) localViewDTO(id, name, ctype string, children int) map[string]any {
	d := folderDTO(id, name, "CollectionFolder", "", children, time.Time{}, s.auth.ServerID())
	d["CollectionType"] = ctype
	delete(d, "ParentId")
	delete(d, "DateCreated")
	d["CanDelete"] = false
	return d
}

// addLocalViews appends the Playlists and Collections libraries when the
// user has playlists or collections exist and the catalog lacks them.
func (s *Server) addLocalViews(ctx context.Context, c *caller, res *catalog.QueryResult) error {
	have := map[string]bool{}
	for _, v := range res.Items {
		have[v["Id"].(string)] = true
	}
	pls, err := s.ud.Playlists(ctx, c.user.ID)
	if err != nil {
		return err
	}
	cols, err := s.ud.Collections(ctx)
	if err != nil {
		return err
	}
	if len(cols) > 0 && !have[collectionsViewID] {
		res.Items = append(res.Items, s.localViewDTO(collectionsViewID, "Collections", "boxsets", len(cols)))
	}
	if len(pls) > 0 && !have[playlistsViewID] {
		res.Items = append(res.Items, s.localViewDTO(playlistsViewID, "Playlists", "playlists", len(pls)))
	}
	res.TotalRecordCount = len(res.Items)
	return nil
}

// localItem returns the DTO for a JellyBloom playlist, collection, or local
// library, or nil.
func (s *Server) localItem(ctx context.Context, c *caller, id string) map[string]any {
	if p, err := s.ud.Playlist(ctx, c.user.ID, id); err == nil {
		return s.playlistDTO(p)
	}
	if col, err := s.ud.Collection(ctx, id); err == nil {
		return s.collectionDTO(col)
	}
	switch id {
	case playlistsViewID:
		return s.localViewDTO(id, "Playlists", "playlists", 0)
	case collectionsViewID:
		return s.localViewDTO(id, "Collections", "boxsets", 0)
	}
	return nil
}

func onlyType(types []string, t string) bool {
	return len(types) > 0 && !slices.ContainsFunc(types, func(x string) bool { return !strings.EqualFold(x, t) })
}

// localItems answers /Items queries about JellyBloom's own lists. It
// reports whether it handled the request.
func (s *Server) localItems(w http.ResponseWriter, r *http.Request, c *caller, q catalog.Query, p params) bool {
	ctx := r.Context()
	parent := identity.NormalizeID(q.ParentID)
	if parent != "" {
		if pl, err := s.ud.Playlist(ctx, c.user.ID, parent); err == nil {
			s.writePlaylistItems(w, r, c, pl)
			return true
		}
		if col, err := s.ud.Collection(ctx, parent); err == nil {
			cq := q
			cq.ParentID, cq.IDs = "", col.Items
			if len(cq.IDs) == 0 {
				writeJSON(w, emptyResult())
				return true
			}
			res, err := s.catalog.Query(ctx, cq, p.listOptions(c))
			s.writeResult(w, res, err)
			return true
		}
	}
	wantPlaylists := parent == playlistsViewID || parent == "" && onlyType(q.IncludeTypes, "Playlist")
	wantCollections := parent == collectionsViewID || parent == "" && onlyType(q.IncludeTypes, "BoxSet")
	if !wantPlaylists && !wantCollections {
		return false
	}

	var items []map[string]any
	if wantPlaylists {
		pls, err := s.ud.Playlists(ctx, c.user.ID)
		if err != nil {
			s.internal(w, err)
			return true
		}
		for _, pl := range pls {
			items = append(items, s.playlistDTO(pl))
		}
	} else {
		cols, err := s.ud.Collections(ctx)
		if err != nil {
			s.internal(w, err)
			return true
		}
		for _, col := range cols {
			items = append(items, s.collectionDTO(col))
		}
		// Upstream collections (BoxSets) in the same library come along.
		uq := q
		uq.StartIndex, uq.Limit = 0, 0
		if parent == collectionsViewID {
			uq.Recursive = true
		}
		uq.IncludeTypes = []string{"BoxSet"}
		up, err := s.catalog.Query(ctx, uq, p.listOptions(c))
		if err != nil {
			s.internal(w, err)
			return true
		}
		items = append(items, up.Items...)
	}
	if term := identity.NormTitle(q.SearchTerm); term != "" {
		items = slices.DeleteFunc(items, func(d map[string]any) bool {
			return !strings.Contains(identity.NormTitle(d["Name"].(string)), term)
		})
	}
	slices.SortStableFunc(items, func(a, b map[string]any) int {
		return strings.Compare(strings.ToLower(a["Name"].(string)), strings.ToLower(b["Name"].(string)))
	})
	writeJSON(w, pageItems(items, q.StartIndex, q.Limit))
	return true
}

func pageItems(items []map[string]any, start, limit int) map[string]any {
	total := len(items)
	start = max(0, min(start, total))
	end := total
	if limit > 0 {
		end = min(start+limit, total)
	}
	page := items[start:end]
	if page == nil {
		page = []map[string]any{}
	}
	return map[string]any{"Items": page, "TotalRecordCount": total, "StartIndex": start}
}

// writePlaylistItems lists a playlist's entries in order, each with its
// PlaylistItemId. Entries whose items are no longer visible are left out.
func (s *Server) writePlaylistItems(w http.ResponseWriter, r *http.Request, c *caller, pl *userdata.Playlist) {
	p := readParams(r)
	var entries []userdata.Entry
	for _, e := range pl.Entries {
		if s.catalog.Known(e.ItemID) {
			entries = append(entries, e)
		}
	}
	start, limit := p.int("startIndex", 0), p.int("limit", 0)
	total := len(entries)
	start = max(0, min(start, total))
	end := total
	if limit > 0 {
		end = min(start+limit, total)
	}
	entries = entries[start:end]
	var ids []string
	for _, e := range entries {
		ids = append(ids, e.ItemID)
	}
	items := []map[string]any{}
	if len(ids) > 0 {
		res, err := s.catalog.Query(r.Context(), catalog.Query{IDs: ids, KeepOrder: true}, p.listOptions(c))
		if err != nil {
			s.internal(w, err)
			return
		}
		items = res.Items
	}
	for i := range items {
		if i < len(entries) {
			items[i]["PlaylistItemId"] = entries[i].ID
		}
	}
	writeJSON(w, map[string]any{"Items": items, "TotalRecordCount": total, "StartIndex": start})
}
