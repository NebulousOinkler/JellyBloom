package server

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
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
		s.listResult(w, r, s.ud.UpdatePlaylist(r.Context(), c.user.ID, pathID(r), b.Name, items))
	}))
	rt.handle(http.MethodGet, "/Playlists/{id}/Items", a(func(w http.ResponseWriter, r *http.Request, c *caller) {
		if p := s.playlist(w, r, c); p != nil {
			s.writePlaylistItems(w, r, c, p)
		}
	}))
	rt.handle(http.MethodPost, "/Playlists/{id}/Items", a(func(w http.ResponseWriter, r *http.Request, c *caller) {
		s.listResult(w, r, s.ud.AddToPlaylist(r.Context(), c.user.ID, pathID(r), s.knownIDs(readParams(r).list("ids"))))
	}))
	rt.handle(http.MethodDelete, "/Playlists/{id}/Items", a(func(w http.ResponseWriter, r *http.Request, c *caller) {
		entries := readParams(r).list("entryIds")
		s.listResult(w, r, s.ud.RemoveFromPlaylist(r.Context(), c.user.ID, pathID(r), normalizeAll(entries)))
	}))
	rt.handle(http.MethodPost, "/Playlists/{id}/Items/{entryId}/Move/{index}", a(func(w http.ResponseWriter, r *http.Request, c *caller) {
		idx, err := parseIndex(r.PathValue("index"))
		if err != nil {
			http.Error(w, "bad index", http.StatusBadRequest)
			return
		}
		s.listResult(w, r, s.ud.MovePlaylistEntry(r.Context(), c.user.ID, pathID(r), identity.NormalizeID(r.PathValue("entryId")), idx))
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
	// Upstream collections are edited in JellyBloom only: additions and
	// removals are kept here and applied over the upstream's members.
	rt.handle(http.MethodPost, "/Collections/{id}/Items", a(func(w http.ResponseWriter, r *http.Request, c *caller) {
		id, ctx := pathID(r), r.Context()
		if !s.isCollection(ctx, id) {
			http.NotFound(w, r)
			return
		}
		s.listResult(w, r, s.ud.AddToCollection(ctx, id, s.knownIDs(readParams(r).list("ids"))))
	}))
	rt.handle(http.MethodDelete, "/Collections/{id}/Items", a(func(w http.ResponseWriter, r *http.Request, c *caller) {
		id, ctx, items := pathID(r), r.Context(), normalizeAll(readParams(r).list("ids"))
		switch {
		case s.isLocalCollection(ctx, id):
			s.listResult(w, r, s.ud.RemoveFromCollection(ctx, id, items))
		case s.catalog.Type(id) == "BoxSet":
			s.listResult(w, r, s.ud.HideInCollection(ctx, id, items))
		default:
			http.NotFound(w, r)
		}
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

// pathID is the {id} path value, normalized.
func pathID(r *http.Request) string { return identity.NormalizeID(r.PathValue("id")) }

// isLocalList: one of c's playlists or a JellyBloom collection.
func (s *Server) isLocalList(ctx context.Context, c *caller, id string) bool {
	_, err := s.ud.Playlist(ctx, c.user.ID, id)
	return err == nil || s.isLocalCollection(ctx, id)
}

// withUserData sets the caller's UserData on JellyBloom's own list DTOs.
func (s *Server) withUserData(ctx context.Context, c *caller, dtos ...map[string]any) {
	ids := make([]string, len(dtos))
	for i, d := range dtos {
		ids[i] = d["Id"].(string)
	}
	uds, err := s.catalog.UserDataDTOs(ctx, c.user.ID, ids)
	if err != nil {
		return // keep the neutral stub
	}
	for i, d := range dtos {
		d["UserData"] = uds[ids[i]]
	}
}

func (s *Server) isLocalCollection(ctx context.Context, id string) bool {
	_, err := s.ud.Collection(ctx, id)
	return err == nil
}

// isCollection: a JellyBloom collection or an upstream one (BoxSet).
func (s *Server) isCollection(ctx context.Context, id string) bool {
	return s.isLocalCollection(ctx, id) || s.catalog.Type(id) == "BoxSet"
}

// collectionMembers lists a JellyBloom or upstream collection's members.
func (s *Server) collectionMembers(r *http.Request, c *caller, id string) ([]string, error) {
	if col, err := s.ud.Collection(r.Context(), id); err == nil {
		return col.Items, nil
	}
	return s.boxSetMembers(r, c, id)
}

// boxSetMembers lists an upstream collection's visible members (merged
// across the upstreams that have it) with JellyBloom's edits applied.
// Jellyfin links members rather than nesting them, so they are asked for.
func (s *Server) boxSetMembers(r *http.Request, c *caller, id string) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	add := func(u string) {
		if u != "" && !seen[u] {
			seen[u] = true
			out = append(out, u)
		}
	}
	for _, cp := range s.catalog.Copies(id) {
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		ids, err := s.upstreamChildren(ctx, r, c, cp.Upstream, cp.ID)
		cancel()
		if err != nil {
			s.log.Warn("listing an upstream collection", "upstream", cp.Upstream, "err", err)
			continue
		}
		for _, real := range ids {
			add(s.catalog.Visible(cp.Upstream, real))
		}
	}
	added, hidden, err := s.ud.CollectionEdits(r.Context(), id)
	if err != nil {
		return nil, err
	}
	for _, a := range added {
		if s.catalog.Known(a) {
			add(a)
		}
	}
	return slices.DeleteFunc(out, func(x string) bool { return slices.Contains(hidden, x) }), nil
}

// upstreamChildren returns the IDs of a folder's children on an upstream.
func (s *Server) upstreamChildren(ctx context.Context, r *http.Request, c *caller, up, parent string) ([]string, error) {
	resp, _, err := s.proxy.Call(ctx, r, up, proxyCaller(c), http.MethodGet, "/Items", url.Values{"ParentId": {parent}}, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	var body struct{ Items []struct{ Id string } }
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(&body); err != nil {
		return nil, err
	}
	ids := make([]string, len(body.Items))
	for i, it := range body.Items {
		ids[i] = it.Id
	}
	return ids, nil
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
		d := s.playlistDTO(p)
		s.withUserData(ctx, c, d)
		return d
	}
	if col, err := s.ud.Collection(ctx, id); err == nil {
		d := s.collectionDTO(col)
		s.withUserData(ctx, c, d)
		return d
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
		if s.isCollection(ctx, parent) {
			members, err := s.collectionMembers(r, c, parent)
			if err != nil {
				s.internal(w, err)
				return true
			}
			if len(members) == 0 {
				writeJSON(w, emptyResult())
				return true
			}
			cq := q
			cq.ParentID, cq.IDs = "", members
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
		s.withUserData(ctx, c, items...)
	} else {
		cols, err := s.ud.Collections(ctx)
		if err != nil {
			s.internal(w, err)
			return true
		}
		for _, col := range cols {
			items = append(items, s.collectionDTO(col))
		}
		s.withUserData(ctx, c, items...)
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
	if by := p.list("sortBy"); len(by) > 0 {
		if slices.ContainsFunc(by, func(k string) bool { return strings.EqualFold(k, "Random") }) {
			rand.Shuffle(len(entries), func(i, j int) { entries[i], entries[j] = entries[j], entries[i] })
		} else {
			ids := make([]string, len(entries))
			for i, e := range entries {
				ids[i] = e.ItemID
			}
			order, err := s.catalog.Order(r.Context(), ids, by, p.list("sortOrder"), c.user.ID)
			if err != nil {
				s.internal(w, err)
				return
			}
			rank := map[string]int{}
			for i, id := range order {
				rank[id] = i
			}
			slices.SortStableFunc(entries, func(a, b userdata.Entry) int { return cmp.Compare(rank[a.ItemID], rank[b.ItemID]) })
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
