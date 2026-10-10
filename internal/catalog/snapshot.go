// Package catalog answers browse queries (libraries, item lists, details,
// search) from the merged index. An in-memory snapshot of the fields needed
// to filter and sort is rebuilt after every identity rebuild; full item
// metadata is read from SQLite only for the items being returned.
package catalog

import (
	"context"
	"database/sql"
	"encoding/json"
	"slices"
	"strings"

	"github.com/NebulousOinkler/JellyBloom/internal/identity"
)

type ref struct{ up, id string }

// node is one visible unified item.
type node struct {
	id, typ, name, sortName, mediaType string
	folder                             bool
	created, premiere                  string // RFC 3339, sortable as strings
	year                               int
	rating, critic                     float64
	runtime                            int64
	index, parentIndex                 int // -1 when absent
	officialRating                     string
	tags                               []string
	series, season, album              string   // unified IDs
	artists, albumArtists              []string // unified MusicArtist IDs
	genres, studios, people            []string // entity IDs
	views                              []string // merged library IDs
	parents                            []string // unified parent IDs (any copy)
	copies                             []ref    // source copies, preferred upstream first
	search                             string   // folded name
}

// view is a merged library: upstream libraries with the same collection type
// and name.
type view struct {
	id, name, collectionType string
	refs                     []ref // upstream libraries, config order
}

// entity is a merged genre, studio, or person.
type entity struct {
	id, typ, name string
	imageTag      string // people: primary image tag of the first copy with one
	refs          []ref
}

type snapshot struct {
	rank     map[string]int // upstream name -> config order
	nodes    map[string]*node
	all      []*node // sorted by ID, for deterministic iteration
	children map[string][]*node
	views    []*view
	viewByID map[string]*view
	entities map[string]*entity
	alias    map[ref]string  // upstream library/genre/studio/person -> synthesized ID
	byRef    map[ref]string  // upstream item copy -> unified ID (all copies)
	hidden   map[string]bool // unified IDs hidden by the set operation
}

func newSnapshot() *snapshot {
	return &snapshot{
		rank: map[string]int{}, nodes: map[string]*node{}, children: map[string][]*node{},
		viewByID: map[string]*view{}, entities: map[string]*entity{}, alias: map[ref]string{},
		byRef: map[ref]string{}, hidden: map[string]bool{},
	}
}

type nameID struct{ Name, Id string }

// header is the subset of a BaseItemDto the snapshot keeps.
type header struct {
	Id, Type, Name, SortName, MediaType string
	IsFolder                            bool
	DateCreated, PremiereDate           string
	ProductionYear                      int
	CommunityRating, CriticRating       float64
	RunTimeTicks                        int64
	IndexNumber, ParentIndexNumber      *int
	OfficialRating                      string
	Tags                                []string
	ParentId, SeriesId, SeasonId        string
	AlbumId                             string
	AlbumArtists, ArtistItems           []nameID
	GenreItems, Studios                 []nameID
	People                              []struct{ Name, Id, Type, PrimaryImageTag string }
}

var musicTypes = map[string]bool{"Audio": true, "MusicAlbum": true, "MusicArtist": true, "MusicVideo": true}

func entityKind(itemType, kind string) string {
	if kind == "Genre" && musicTypes[itemType] {
		return "MusicGenre"
	}
	return kind
}

func entityID(kind, name string) string {
	return identity.DeriveID(strings.ToLower(kind) + ":" + identity.NormTitle(name))
}

// build reads the index and identity tables into a snapshot, returning the
// aliases synthesized IDs need for routing.
func build(ctx context.Context, db *sql.DB, upstreams []string) (*snapshot, []identity.Alias, error) {
	s := newSnapshot()
	for i, u := range upstreams {
		s.rank[u] = i
	}
	byRank := func(a, b ref) int { return s.rank[a.up] - s.rank[b.up] }

	// Libraries, grouped by collection type and folded name.
	rows, err := db.QueryContext(ctx, `SELECT upstream, real_id, name, collection_type FROM views`)
	if err != nil {
		return nil, nil, err
	}
	type upView struct {
		r          ref
		name, kind string
	}
	var upViews []upView
	for rows.Next() {
		var v upView
		if err := rows.Scan(&v.r.up, &v.r.id, &v.name, &v.kind); err != nil {
			rows.Close()
			return nil, nil, err
		}
		v.r.id = identity.NormalizeID(v.r.id)
		upViews = append(upViews, v)
	}
	rows.Close()
	slices.SortFunc(upViews, func(a, b upView) int {
		if d := byRank(a.r, b.r); d != 0 {
			return d
		}
		return strings.Compare(a.name, b.name)
	})
	for _, v := range upViews {
		id := identity.DeriveID("view:" + v.kind + ":" + identity.NormTitle(v.name))
		vw := s.viewByID[id]
		if vw == nil {
			vw = &view{id: id, name: v.name, collectionType: v.kind}
			s.viewByID[id] = vw
			s.views = append(s.views, vw)
		}
		vw.refs = append(vw.refs, v.r)
		s.alias[v.r] = id
	}

	// Every copy's unified ID.
	rows, err = db.QueryContext(ctx, `SELECT upstream, real_id, unified_id FROM mappings`)
	if err != nil {
		return nil, nil, err
	}
	for rows.Next() {
		var r ref
		var u string
		if err := rows.Scan(&r.up, &r.id, &u); err != nil {
			rows.Close()
			return nil, nil, err
		}
		s.byRef[r] = u
	}
	rows.Close()

	// Visible items, from their source copies.
	type copyHdr struct {
		r ref
		h header
	}
	copies := map[string][]copyHdr{}
	rows, err = db.QueryContext(ctx, `
		SELECT i.upstream, i.real_id, i.dto FROM items i
		JOIN mappings m ON m.upstream = i.upstream AND m.real_id = i.real_id
		JOIN unified_items u ON u.unified_id = m.unified_id
		WHERE m.is_source = 1 AND u.visible = 1`)
	if err != nil {
		return nil, nil, err
	}
	for rows.Next() {
		var c copyHdr
		var dto []byte
		if err := rows.Scan(&c.r.up, &c.r.id, &dto); err != nil {
			rows.Close()
			return nil, nil, err
		}
		if json.Unmarshal(dto, &c.h) != nil {
			continue
		}
		u := s.byRef[c.r]
		copies[u] = append(copies[u], c)
	}
	rows.Close()

	unified := func(up, real string) string {
		if real == "" {
			return ""
		}
		return s.byRef[ref{up, identity.NormalizeID(real)}]
	}
	addEntity := func(kind string, c copyHdr, name, realID, imageTag string) string {
		if name == "" {
			return ""
		}
		id := entityID(kind, name)
		e := s.entities[id]
		if e == nil {
			e = &entity{id: id, typ: kind, name: name}
			s.entities[id] = e
		}
		if e.imageTag == "" {
			e.imageTag = imageTag
		}
		if realID != "" {
			r := ref{c.r.up, identity.NormalizeID(realID)}
			if _, ok := s.alias[r]; !ok {
				s.alias[r] = id
				e.refs = append(e.refs, r)
			}
		}
		return id
	}
	addUnique := func(list []string, v string) []string {
		if v != "" && !slices.Contains(list, v) {
			return append(list, v)
		}
		return list
	}

	for u, cs := range copies {
		slices.SortFunc(cs, func(a, b copyHdr) int { return byRank(a.r, b.r) })
		h := cs[0].h // preferred copy supplies scalar fields
		n := &node{
			id: u, typ: h.Type, name: h.Name, sortName: strings.ToLower(h.SortName), mediaType: h.MediaType,
			folder: h.IsFolder, created: h.DateCreated, premiere: h.PremiereDate, year: h.ProductionYear,
			rating: h.CommunityRating, critic: h.CriticRating, runtime: h.RunTimeTicks,
			index: -1, parentIndex: -1, officialRating: h.OfficialRating, tags: h.Tags,
			series: unified(cs[0].r.up, h.SeriesId), season: unified(cs[0].r.up, h.SeasonId),
			album: unified(cs[0].r.up, h.AlbumId), search: identity.NormTitle(h.Name),
		}
		if n.sortName == "" {
			n.sortName = strings.ToLower(h.Name)
		}
		if h.IndexNumber != nil {
			n.index = *h.IndexNumber
		}
		if h.ParentIndexNumber != nil {
			n.parentIndex = *h.ParentIndexNumber
		}
		for _, c := range cs {
			n.copies = append(n.copies, c.r)
			if p := identity.NormalizeID(c.h.ParentId); p != "" {
				if pu := s.byRef[ref{c.r.up, p}]; pu != "" && pu != u {
					n.parents = addUnique(n.parents, pu)
				}
			}
			for _, a := range c.h.AlbumArtists {
				n.albumArtists = addUnique(n.albumArtists, unified(c.r.up, a.Id))
				n.artists = addUnique(n.artists, unified(c.r.up, a.Id))
			}
			for _, a := range c.h.ArtistItems {
				n.artists = addUnique(n.artists, unified(c.r.up, a.Id))
			}
			for _, g := range c.h.GenreItems {
				n.genres = addUnique(n.genres, addEntity(entityKind(h.Type, "Genre"), c, g.Name, g.Id, ""))
			}
			for _, st := range c.h.Studios {
				n.studios = addUnique(n.studios, addEntity("Studio", c, st.Name, st.Id, ""))
			}
			for _, p := range c.h.People {
				n.people = addUnique(n.people, addEntity("Person", c, p.Name, p.Id, p.PrimaryImageTag))
			}
		}
		s.nodes[u] = n
	}

	// Library membership.
	rows, err = db.QueryContext(ctx, `SELECT upstream, real_id, view_id FROM item_views`)
	if err != nil {
		return nil, nil, err
	}
	for rows.Next() {
		var r ref
		var v string
		if err := rows.Scan(&r.up, &r.id, &v); err != nil {
			rows.Close()
			return nil, nil, err
		}
		n := s.nodes[s.byRef[r]]
		vid := s.alias[ref{r.up, identity.NormalizeID(v)}]
		if n != nil && vid != "" && slices.Contains(n.copies, r) {
			n.views = addUnique(n.views, vid)
		}
	}
	rows.Close()

	for _, u := range s.byRef {
		if s.nodes[u] == nil {
			s.hidden[u] = true
		}
	}
	for _, n := range s.nodes {
		s.all = append(s.all, n)
		for _, p := range n.parents {
			s.children[p] = append(s.children[p], n)
		}
	}
	slices.SortFunc(s.all, func(a, b *node) int { return strings.Compare(a.id, b.id) })
	for _, kids := range s.children {
		slices.SortFunc(kids, func(a, b *node) int { return strings.Compare(a.id, b.id) })
	}

	alias := func(id string, refs []ref) identity.Alias {
		a := identity.Alias{ID: id}
		for _, r := range refs {
			a.Refs = append(a.Refs, identity.Ref{Upstream: r.up, ID: r.id})
		}
		return a
	}
	var aliases []identity.Alias
	for _, v := range s.views {
		aliases = append(aliases, alias(v.id, v.refs))
	}
	for _, e := range s.entities {
		if len(e.refs) > 0 {
			slices.SortStableFunc(e.refs, byRank)
			aliases = append(aliases, alias(e.id, e.refs))
		}
	}
	return s, aliases, nil
}

// topLevel reports whether n is a top-level item of view v: in the library, and
// with no parent that is also in it.
func (s *snapshot) topLevel(n *node, v string) bool {
	if !slices.Contains(n.views, v) {
		return false
	}
	for _, p := range n.parents {
		if pn := s.nodes[p]; pn != nil && slices.Contains(pn.views, v) {
			return false
		}
	}
	return true
}

// descendants returns every node below id (excluding id).
func (s *snapshot) descendants(id string) []*node {
	var out []*node
	seen := map[string]bool{id: true}
	stack := []string{id}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		for _, c := range s.children[cur] {
			if !seen[c.id] {
				seen[c.id] = true
				out = append(out, c)
				stack = append(stack, c.id)
			}
		}
	}
	return out
}
