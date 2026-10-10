package catalog

import (
	"cmp"
	"math/rand/v2"
	"slices"
	"strings"

	"github.com/NebulousOinkler/JellyBloom/internal/identity"
)

// Query is the subset of Jellyfin's /Items parameters JellyBloom answers.
type Query struct {
	ParentID     string
	Recursive    bool
	IDs          []string
	IncludeTypes []string
	ExcludeTypes []string
	MediaTypes   []string
	SearchTerm   string
	SortBy       []string
	SortOrder    []string // per SortBy entry; the last applies to the rest
	StartIndex   int
	Limit        int // <= 0: no limit

	GenreIDs, StudioIDs, PersonIDs []string
	ArtistIDs, AlbumArtistIDs      []string
	AlbumIDs                       []string
	Genres                         []string // names
	Years                          []int
	IsFolder                       *bool
	NameStartsWith                 string
	NameStartsWithOrGreater        string
	NameLessThan                   string
	Filters                        []string
	SeriesID, SeasonID             string
	ParentIndexNumber              *int
}

// Result is a page of unified IDs.
type Result struct {
	IDs        []string
	Total      int
	StartIndex int
}

func hasFold(list []string, v string) bool {
	return slices.ContainsFunc(list, func(s string) bool { return strings.EqualFold(s, v) })
}

func anyIn(have, want []string) bool {
	for _, w := range want {
		if slices.Contains(have, identity.NormalizeID(w)) || slices.Contains(have, w) {
			return true
		}
	}
	return false
}

// candidates returns the nodes in scope of the parent/recursive parameters.
func (s *snapshot) candidates(q Query) []*node {
	switch {
	case len(q.IDs) > 0:
		var out []*node
		for _, id := range q.IDs {
			if n := s.nodes[identity.NormalizeID(id)]; n != nil {
				out = append(out, n)
			}
		}
		return out
	case q.ParentID == "":
		return s.all
	}
	parent := identity.NormalizeID(q.ParentID)
	if _, ok := s.viewByID[parent]; ok {
		var out []*node
		for _, n := range s.all {
			if q.Recursive && slices.Contains(n.views, parent) || !q.Recursive && s.topLevel(n, parent) {
				out = append(out, n)
			}
		}
		return out
	}
	if e := s.entities[parent]; e != nil { // e.g. parentId=<genre> lists its items
		var out []*node
		for _, n := range s.all {
			if slices.Contains(n.genres, parent) || slices.Contains(n.studios, parent) || slices.Contains(n.people, parent) {
				out = append(out, n)
			}
		}
		return out
	}
	if q.Recursive {
		return s.descendants(parent)
	}
	return s.children[parent]
}

func (q Query) match(s *snapshot, n *node) bool {
	if len(q.IncludeTypes) > 0 && !hasFold(q.IncludeTypes, n.typ) {
		return false
	}
	if hasFold(q.ExcludeTypes, n.typ) {
		return false
	}
	if len(q.MediaTypes) > 0 && !hasFold(q.MediaTypes, n.mediaType) {
		return false
	}
	if q.SearchTerm != "" && !strings.Contains(n.search, identity.NormTitle(q.SearchTerm)) {
		return false
	}
	if q.IsFolder != nil && n.folder != *q.IsFolder {
		return false
	}
	for _, f := range [][2][]string{{q.GenreIDs, n.genres}, {q.StudioIDs, n.studios}, {q.PersonIDs, n.people},
		{q.ArtistIDs, n.artists}, {q.AlbumArtistIDs, n.albumArtists}} {
		if len(f[0]) > 0 && !anyIn(f[1], f[0]) {
			return false
		}
	}
	if len(q.AlbumIDs) > 0 && !anyIn([]string{n.album}, q.AlbumIDs) {
		return false
	}
	if len(q.Genres) > 0 {
		ok := false
		for _, g := range q.Genres {
			for _, kind := range []string{"Genre", "MusicGenre"} {
				if slices.Contains(n.genres, entityID(kind, g)) {
					ok = true
				}
			}
		}
		if !ok {
			return false
		}
	}
	if len(q.Years) > 0 && !slices.Contains(q.Years, n.year) {
		return false
	}
	if q.NameStartsWith != "" && !strings.HasPrefix(n.sortName, strings.ToLower(q.NameStartsWith)) {
		return false
	}
	if q.NameStartsWithOrGreater != "" && n.sortName < strings.ToLower(q.NameStartsWithOrGreater) {
		return false
	}
	if q.NameLessThan != "" && n.sortName >= strings.ToLower(q.NameLessThan) {
		return false
	}
	if q.SeriesID != "" && n.series != identity.NormalizeID(q.SeriesID) {
		return false
	}
	if q.SeasonID != "" && n.season != identity.NormalizeID(q.SeasonID) {
		return false
	}
	if q.ParentIndexNumber != nil && n.parentIndex != *q.ParentIndexNumber {
		return false
	}
	for _, f := range q.Filters {
		switch strings.ToLower(f) {
		case "isfolder":
			if !n.folder {
				return false
			}
		case "isnotfolder":
			if n.folder {
				return false
			}
		case "isfavorite", "isfavoriteorlikes", "likes", "isresumable", "isplayed", "dislikes":
			return false // no user data yet (phase 7): nothing qualifies
		}
	}
	return true
}

type less func(a, b *node) int

var sorters = map[string]less{
	"sortname":          func(a, b *node) int { return strings.Compare(a.sortName, b.sortName) },
	"name":              func(a, b *node) int { return strings.Compare(strings.ToLower(a.name), strings.ToLower(b.name)) },
	"datecreated":       func(a, b *node) int { return strings.Compare(a.created, b.created) },
	"premieredate":      func(a, b *node) int { return strings.Compare(a.premiere, b.premiere) },
	"productionyear":    func(a, b *node) int { return cmp.Compare(a.year, b.year) },
	"communityrating":   func(a, b *node) int { return cmp.Compare(a.rating, b.rating) },
	"criticrating":      func(a, b *node) int { return cmp.Compare(a.critic, b.critic) },
	"runtime":           func(a, b *node) int { return cmp.Compare(a.runtime, b.runtime) },
	"officialrating":    func(a, b *node) int { return strings.Compare(a.officialRating, b.officialRating) },
	"indexnumber":       func(a, b *node) int { return cmp.Compare(a.index, b.index) },
	"parentindexnumber": func(a, b *node) int { return cmp.Compare(a.parentIndex, b.parentIndex) },
}

// run filters, sorts, and pages.
func (s *snapshot) run(q Query) Result {
	var hits []*node
	for _, n := range s.candidates(q) {
		if q.match(s, n) {
			hits = append(hits, n)
		}
	}
	s.sort(hits, q.SortBy, q.SortOrder)
	return page(hits, q.StartIndex, q.Limit)
}

func (s *snapshot) sort(hits []*node, by, order []string) {
	if slices.ContainsFunc(by, func(k string) bool { return strings.EqualFold(k, "Random") }) {
		rand.Shuffle(len(hits), func(i, j int) { hits[i], hits[j] = hits[j], hits[i] })
		return
	}
	type key struct {
		f    less
		desc bool
	}
	var keys []key
	for i, k := range by {
		f := sorters[strings.ToLower(k)]
		if f == nil {
			continue // unsupported keys (e.g. DatePlayed before phase 7) are ignored
		}
		desc := false
		if len(order) > 0 {
			desc = strings.EqualFold(order[min(i, len(order)-1)], "Descending")
		}
		keys = append(keys, key{f, desc})
	}
	keys = append(keys, key{sorters["sortname"], false}, key{func(a, b *node) int { return strings.Compare(a.id, b.id) }, false})
	slices.SortStableFunc(hits, func(a, b *node) int {
		for _, k := range keys {
			if d := k.f(a, b); d != 0 {
				if k.desc {
					return -d
				}
				return d
			}
		}
		return 0
	})
}

func page(hits []*node, start, limit int) Result {
	r := Result{Total: len(hits), StartIndex: start}
	if start < 0 {
		start = 0
	}
	end := len(hits)
	if limit > 0 {
		end = min(start+limit, end)
	}
	for i := start; i < end; i++ {
		r.IDs = append(r.IDs, hits[i].id)
	}
	return r
}

// latest returns recently added items under parent, newest first. With group,
// episodes collapse into their series and tracks into their album.
func (s *snapshot) latest(parent string, types []string, group bool, limit int) []string {
	q := Query{ParentID: parent, Recursive: true, IncludeTypes: types}
	f := false
	q.IsFolder = &f
	var hits []*node
	for _, n := range s.candidates(q) {
		if q.match(s, n) {
			hits = append(hits, n)
		}
	}
	s.sort(hits, []string{"DateCreated"}, []string{"Descending"})
	var out []string
	seen := map[string]bool{}
	for _, n := range hits {
		id := n.id
		if group {
			switch {
			case n.typ == "Episode" && s.nodes[n.series] != nil:
				id = n.series
			case n.typ == "Audio" && s.nodes[n.album] != nil:
				id = n.album
			}
		}
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
			if limit > 0 && len(out) >= limit {
				break
			}
		}
	}
	return out
}

// splitNames separates name filters, which on /Artists, /Genres, etc. apply
// to the listed names rather than to the items they are credited on.
func (q Query) splitNames() (items Query, names func(name string) bool) {
	term, starts, startsOrGreater, less := identity.NormTitle(q.SearchTerm), strings.ToLower(q.NameStartsWith),
		strings.ToLower(q.NameStartsWithOrGreater), strings.ToLower(q.NameLessThan)
	items = q
	items.SearchTerm, items.NameStartsWith, items.NameStartsWithOrGreater, items.NameLessThan = "", "", "", ""
	return items, func(name string) bool {
		l := strings.ToLower(name)
		return (term == "" || strings.Contains(identity.NormTitle(name), term)) &&
			(starts == "" || strings.HasPrefix(l, starts)) &&
			(startsOrGreater == "" || l >= startsOrGreater) &&
			(less == "" || l < less)
	}
}

// entityPage lists entities of the given kinds attached to items matching q,
// sorted by name.
func (s *snapshot) entityPage(q Query, kinds ...string) Result {
	q, nameOK := q.splitNames()
	var hits []*node
	for _, n := range s.candidates(q) {
		if q.match(s, n) {
			hits = append(hits, n)
		}
	}
	seen := map[string]bool{}
	var ents []*entity
	for _, n := range hits {
		for _, list := range [][]string{n.genres, n.studios, n.people} {
			for _, id := range list {
				if e := s.entities[id]; e != nil && !seen[id] && slices.Contains(kinds, e.typ) && nameOK(e.name) {
					seen[id] = true
					ents = append(ents, e)
				}
			}
		}
	}
	slices.SortFunc(ents, func(a, b *entity) int {
		return cmp.Or(strings.Compare(strings.ToLower(a.name), strings.ToLower(b.name)), strings.Compare(a.id, b.id))
	})
	r := Result{Total: len(ents), StartIndex: q.StartIndex}
	end := len(ents)
	if q.Limit > 0 {
		end = min(q.StartIndex+q.Limit, end)
	}
	for i := max(q.StartIndex, 0); i < end; i++ {
		r.IDs = append(r.IDs, ents[i].id)
	}
	return r
}

// artistPage lists music artists credited on items matching q. With
// albumArtists, only album-artist credits count.
func (s *snapshot) artistPage(q Query, albumArtists bool) Result {
	q, nameOK := q.splitNames()
	var hits []*node
	for _, n := range s.candidates(q) {
		if q.match(s, n) {
			hits = append(hits, n)
		}
	}
	seen := map[string]bool{}
	var artists []*node
	add := func(id string) {
		if a := s.nodes[id]; a != nil && !seen[id] && nameOK(a.name) {
			seen[id] = true
			artists = append(artists, a)
		}
	}
	for _, n := range hits {
		if n.typ == "MusicArtist" {
			add(n.id)
		}
		list := n.artists
		if albumArtists {
			list = n.albumArtists
		}
		for _, a := range list {
			add(a)
		}
	}
	s.sort(artists, q.SortBy, q.SortOrder)
	return page(artists, q.StartIndex, q.Limit)
}
