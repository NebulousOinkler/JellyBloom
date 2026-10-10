package catalog

import (
	"bytes"
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/NebulousOinkler/JellyBloom/internal/identity"
	"github.com/NebulousOinkler/JellyBloom/internal/rewrite"
	"github.com/NebulousOinkler/JellyBloom/internal/userdata"
)

// Resolver is the identity API the catalog needs; *identity.Resolver implements it.
type Resolver interface {
	SetAliases(ctx context.Context, aliases []identity.Alias) error
	UnifiedID(ctx context.Context, upstream, realID string) (string, error)
}

// Options configures a Catalog.
type Options struct {
	DB        *sql.DB
	Resolver  Resolver
	Upstreams []string // config order: earlier upstreams win metadata conflicts
	ServerID  string   // JellyBloom's server ID
	// UpstreamIDs returns the user and server IDs JellyBloom has on an
	// upstream, so DTOs can map them to JellyBloom's.
	UpstreamIDs func(ctx context.Context, upstream string) (userID, serverID string)
	// UserData supplies per-user item state; *userdata.Store implements it.
	// Without it, items carry neutral user data.
	UserData UserData
	Logger   *slog.Logger
}

// UserData is the per-user state the catalog overlays and filters by.
type UserData interface {
	Get(ctx context.Context, userID string, ids []string) (map[string]userdata.Record, error)
	All(ctx context.Context, userID string) (map[string]userdata.Record, error)
}

// Catalog answers browse queries. It is safe for concurrent use.
type Catalog struct {
	o    Options
	log  *slog.Logger
	snap atomic.Pointer[snapshot]
}

// New creates a catalog with an empty snapshot; call Reload to fill it.
func New(o Options) *Catalog {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.UpstreamIDs == nil {
		o.UpstreamIDs = func(context.Context, string) (string, string) { return "", "" }
	}
	c := &Catalog{o: o, log: o.Logger}
	c.snap.Store(newSnapshot())
	return c
}

// Reload rebuilds the snapshot from the database and registers routing
// aliases for synthesized IDs.
func (c *Catalog) Reload(ctx context.Context) error {
	start := time.Now()
	s, aliases, err := build(ctx, c.o.DB, c.o.Upstreams)
	if err != nil {
		return err
	}
	if err := c.o.Resolver.SetAliases(ctx, aliases); err != nil {
		return err
	}
	c.snap.Store(s)
	c.log.Info("catalog reloaded", "items", len(s.nodes), "libraries", len(s.views), "entities", len(s.entities),
		"took", time.Since(start).Round(time.Millisecond))
	return nil
}

// Counts returns the number of visible items and libraries.
func (c *Catalog) Counts() (items, libraries int) {
	s := c.snap.Load()
	return len(s.nodes), len(s.views)
}

// Hidden reports whether a unified ID names an item hidden by the set
// operation. Unknown IDs are not hidden.
func (c *Catalog) Hidden(id string) bool {
	return c.snap.Load().hidden[identity.NormalizeID(id)]
}

// DTOOptions shapes returned items.
type DTOOptions struct {
	Fields []string // optional fields to include; nil includes all
	UserID string   // JellyBloom user the response is for
}

// QueryResult is Jellyfin's paged envelope.
type QueryResult struct {
	Items            []map[string]any
	TotalRecordCount int
	StartIndex       int
}

func (c *Catalog) result(ctx context.Context, s *snapshot, r Result, o DTOOptions) (*QueryResult, error) {
	items, err := c.dtos(ctx, s, r.IDs, o)
	if err != nil {
		return nil, err
	}
	if items == nil {
		items = []map[string]any{}
	}
	return &QueryResult{Items: items, TotalRecordCount: r.Total, StartIndex: max(r.StartIndex, 0)}, nil
}

// Views returns the merged libraries.
func (c *Catalog) Views(ctx context.Context, o DTOOptions) (*QueryResult, error) {
	s := c.snap.Load()
	// Libraries whose items are all hidden by the set operation are omitted.
	nonEmpty := map[string]bool{}
	for _, n := range s.all {
		for _, v := range n.views {
			nonEmpty[v] = true
		}
	}
	var ids []string
	for _, v := range s.views {
		if nonEmpty[v.id] {
			ids = append(ids, v.id)
		}
	}
	return c.result(ctx, s, Result{IDs: ids, Total: len(ids)}, o)
}

// Query runs an /Items query.
func (c *Catalog) Query(ctx context.Context, q Query, o DTOOptions) (*QueryResult, error) {
	s := c.snap.Load()
	if q.needsUser() {
		var err error
		if q.user, err = c.allUserData(ctx, o.UserID); err != nil {
			return nil, err
		}
	}
	return c.result(ctx, s, s.run(q), o)
}

// allUserData loads a user's records (nil when there is no source or user).
func (c *Catalog) allUserData(ctx context.Context, userID string) (map[string]userdata.Record, error) {
	if c.o.UserData == nil || userID == "" {
		return nil, nil
	}
	return c.o.UserData.All(ctx, userID)
}

// Resume lists items the user can resume, most recently played first.
func (c *Catalog) Resume(ctx context.Context, q Query, o DTOOptions) (*QueryResult, error) {
	s := c.snap.Load()
	user, err := c.allUserData(ctx, o.UserID)
	if err != nil {
		return nil, err
	}
	q.Recursive, q.user = true, user
	var hits []*node
	for _, n := range s.candidates(q) {
		if user[n.id].Resumable() && q.match(s, n) {
			hits = append(hits, n)
		}
	}
	s.sort(hits, []string{"DatePlayed"}, []string{"Descending"}, q)
	return c.result(ctx, s, page(hits, q.StartIndex, q.Limit), o)
}

// NextUpOptions shapes NextUp (Jellyfin's /Shows/NextUp parameters).
type NextUpOptions struct {
	SeriesID, ParentID string
	StartIndex, Limit  int
	SkipResumable      bool      // enableResumable=false: a series whose next episode is in progress offers nothing
	Rewatching         bool      // enableRewatching: also offer the next played episode after the most recently played one
	Cutoff             time.Time // nextUpDateCutoff: leave out series last watched before this
}

// NextUp follows Jellyfin 12.2 (checked against a real server). The series
// are those with any episode the user has played or started (last watched
// at or after Cutoff), most recently watched first; with SeriesID, just that
// one. Each offers the first unplayed episode after its furthest played
// one, or its first unplayed episode when none is played. Specials
// (season 0) are never offered. With Rewatching, each series also offers
// the next played episode after its most recently played one, and all
// offers are ordered by when their anchor episode was played.
func (c *Catalog) NextUp(ctx context.Context, n NextUpOptions, o DTOOptions) (*QueryResult, error) {
	s := c.snap.Load()
	user, err := c.allUserData(ctx, o.UserID)
	if err != nil {
		return nil, err
	}
	type series struct {
		id     string
		eps    []*node // in order, specials excluded
		recent time.Time
	}
	byID := map[string]*series{}
	for _, e := range s.all {
		if e.typ != "Episode" || e.series == "" {
			continue
		}
		sr := byID[e.series]
		if sr == nil {
			sr = &series{id: e.series}
			byID[e.series] = sr
		}
		if t := user[e.id].LastPlayed; t.After(sr.recent) {
			sr.recent = t
		}
		if e.parentIndex != 0 {
			sr.eps = append(sr.eps, e)
		}
	}
	var keys []*series
	for _, sr := range byID {
		switch {
		case n.SeriesID != "":
			if sr.id != identity.NormalizeID(n.SeriesID) {
				continue
			}
		case sr.recent.IsZero() || sr.recent.Before(n.Cutoff):
			continue
		}
		if n.ParentID != "" {
			if sn := s.nodes[sr.id]; sn == nil || !slices.Contains(sn.views, identity.NormalizeID(n.ParentID)) {
				continue
			}
		}
		slices.SortFunc(sr.eps, func(a, b *node) int {
			return cmp.Or(cmp.Compare(a.parentIndex, b.parentIndex), cmp.Compare(a.index, b.index))
		})
		keys = append(keys, sr)
	}
	slices.SortFunc(keys, func(a, b *series) int { return cmp.Or(b.recent.Compare(a.recent), strings.Compare(a.id, b.id)) })

	type pick struct {
		ep   *node
		when time.Time // the anchor episode's last played date
	}
	// next offers the first episode after anchor (-1: from the start) whose
	// played state is played, unless it is in progress and SkipResumable.
	next := func(eps []*node, anchor int, played bool) *node {
		for _, e := range eps[anchor+1:] {
			if r := user[e.id]; r.Played == played {
				if n.SkipResumable && r.Resumable() {
					return nil
				}
				return e
			}
		}
		return nil
	}
	var picks []pick
	for _, sr := range keys {
		furthest := -1
		for i, e := range sr.eps {
			if user[e.id].Played {
				furthest = i
			}
		}
		var when time.Time
		if furthest >= 0 {
			when = user[sr.eps[furthest].id].LastPlayed
		}
		if e := next(sr.eps, furthest, false); e != nil {
			picks = append(picks, pick{e, when})
		}
	}
	if n.Rewatching {
		for _, sr := range keys {
			recent := -1
			for i, e := range sr.eps { // latest play wins; ties go to the later episode
				if r := user[e.id]; r.Played && (recent < 0 || !r.LastPlayed.Before(user[sr.eps[recent].id].LastPlayed)) {
					recent = i
				}
			}
			if recent < 0 {
				continue
			}
			if e := next(sr.eps, recent, true); e != nil {
				picks = append(picks, pick{e, user[sr.eps[recent].id].LastPlayed})
			}
		}
		slices.SortStableFunc(picks, func(a, b pick) int { return b.when.Compare(a.when) })
	}
	var hits []*node
	for _, p := range picks {
		hits = append(hits, p.ep)
	}
	return c.result(ctx, s, page(hits, n.StartIndex, n.Limit), o)
}

// Known reports whether id is a visible item.
func (c *Catalog) Known(id string) bool { return c.snap.Load().nodes[identity.NormalizeID(id)] != nil }

// Leaves returns the playable items at or below id (id itself for a leaf):
// marking a series played marks its episodes.
func (c *Catalog) Leaves(id string) []string {
	var out []string
	for _, n := range c.snap.Load().leaves(identity.NormalizeID(id)) {
		out = append(out, n.id)
	}
	return out
}

// MediaType returns an item's media type ("Video", "Audio", …).
func (c *Catalog) MediaType(id string) string {
	if n := c.snap.Load().nodes[identity.NormalizeID(id)]; n != nil {
		return n.mediaType
	}
	return ""
}

// Type returns an item's Jellyfin type ("Movie", "BoxSet", …), or "".
func (c *Catalog) Type(id string) string {
	if n := c.snap.Load().nodes[identity.NormalizeID(id)]; n != nil {
		return n.typ
	}
	return ""
}

// Copies returns the upstream copies of a visible item, preferred first.
func (c *Catalog) Copies(id string) []identity.Ref {
	n := c.snap.Load().nodes[identity.NormalizeID(id)]
	if n == nil {
		return nil
	}
	out := make([]identity.Ref, len(n.copies))
	for i, r := range n.copies {
		out[i] = identity.Ref{Upstream: r.up, ID: r.id, Source: true}
	}
	return out
}

// Visible returns the visible unified ID of an upstream item, or "".
func (c *Catalog) Visible(upstream, realID string) string {
	s := c.snap.Load()
	u := s.byRef[ref{upstream, identity.NormalizeID(realID)}]
	if s.nodes[u] == nil {
		return ""
	}
	return u
}

// Order returns the distinct visible ids in the order a query with these
// sort keys would list them; user-data keys use userID's data.
func (c *Catalog) Order(ctx context.Context, ids, sortBy, sortOrder []string, userID string) ([]string, error) {
	q := Query{IDs: slices.Compact(slices.Sorted(slices.Values(ids))), SortBy: sortBy, SortOrder: sortOrder}
	if q.needsUser() {
		var err error
		if q.user, err = c.allUserData(ctx, userID); err != nil {
			return nil, err
		}
	}
	return c.snap.Load().run(q).IDs, nil
}

// PlaybackItem returns what playback rules need to know about an item.
func (c *Catalog) PlaybackItem(id string) userdata.Item {
	n := c.snap.Load().nodes[identity.NormalizeID(id)]
	switch {
	case n == nil:
		return userdata.Item{}
	case n.typ == "AudioBook":
		return userdata.Item{Runtime: n.runtime, Kind: userdata.KindAudioBook}
	case n.mediaType == "Audio":
		return userdata.Item{Runtime: n.runtime, Kind: userdata.KindAudio}
	}
	return userdata.Item{Runtime: n.runtime, Kind: userdata.KindVideo}
}

// Runtime returns an item's runtime in ticks (0 if unknown).
func (c *Catalog) Runtime(id string) int64 {
	if n := c.snap.Load().nodes[identity.NormalizeID(id)]; n != nil {
		return n.runtime
	}
	return 0
}

// UserDataDTOs renders the user's data for ids, folders included.
func (c *Catalog) UserDataDTOs(ctx context.Context, userID string, ids []string) (map[string]map[string]any, error) {
	return c.userDataDTOs(ctx, c.snap.Load(), userID, ids)
}

// userDataDTOs builds UserItemDataDtos; folders get unplayed counts over
// their playable items.
func (c *Catalog) userDataDTOs(ctx context.Context, s *snapshot, userID string, ids []string) (map[string]map[string]any, error) {
	out := map[string]map[string]any{}
	if c.o.UserData == nil || userID == "" {
		for _, id := range ids {
			out[id] = userDataStub(id)
		}
		return out, nil
	}
	need := slices.Clone(ids)
	leaves := map[string][]*node{}
	for _, id := range ids {
		if n := s.nodes[id]; n != nil && n.folder {
			leaves[id] = s.leaves(id)
			for _, l := range leaves[id] {
				need = append(need, l.id)
			}
		}
	}
	// Many leaves (a page of series): one scan of the user's records beats
	// hundreds of chunked lookups.
	get := c.o.UserData.Get
	if len(need) > 2000 {
		get = func(ctx context.Context, userID string, _ []string) (map[string]userdata.Record, error) {
			return c.o.UserData.All(ctx, userID)
		}
	}
	recs, err := get(ctx, userID, need)
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		var runtime int64
		if n := s.nodes[id]; n != nil {
			runtime = n.runtime
		}
		r := recs[id]
		if ls, folder := leaves[id]; folder {
			unplayed := 0
			for _, l := range ls {
				if !recs[l.id].Played {
					unplayed++
				}
			}
			r.Played = len(ls) > 0 && unplayed == 0
			d := r.DTO(id, 0)
			d["UnplayedItemCount"] = unplayed
			if len(ls) > 0 {
				d["PlayedPercentage"] = float64(len(ls)-unplayed) / float64(len(ls)) * 100
			}
			out[id] = d
			continue
		}
		out[id] = r.DTO(id, runtime)
	}
	return out, nil
}

// Item returns one item (library, entity, or item), or nil if unknown or hidden.
func (c *Catalog) Item(ctx context.Context, id string, o DTOOptions) (map[string]any, error) {
	s := c.snap.Load()
	items, err := c.dtos(ctx, s, []string{identity.NormalizeID(id)}, o)
	if err != nil || len(items) == 0 {
		return nil, err
	}
	return items[0], nil
}

// Latest returns recently added items under parent; with hidePlayed, items
// the user has played are left out (Jellyfin's HidePlayedInLatest).
func (c *Catalog) Latest(ctx context.Context, parent string, types []string, group, hidePlayed bool, limit int, o DTOOptions) ([]map[string]any, error) {
	s := c.snap.Load()
	var skip map[string]userdata.Record
	if hidePlayed {
		var err error
		if skip, err = c.allUserData(ctx, o.UserID); err != nil {
			return nil, err
		}
	}
	items, err := c.dtos(ctx, s, s.latest(parent, types, group, limit, skip), o)
	if items == nil {
		items = []map[string]any{}
	}
	return items, err
}

// Artists lists artists credited on items matching q.
func (c *Catalog) Artists(ctx context.Context, q Query, albumArtists bool, o DTOOptions) (*QueryResult, error) {
	s := c.snap.Load()
	return c.result(ctx, s, s.artistPage(q, albumArtists), o)
}

// Entities lists genres, studios, or people attached to items matching q.
func (c *Catalog) Entities(ctx context.Context, q Query, o DTOOptions, kinds ...string) (*QueryResult, error) {
	s := c.snap.Load()
	return c.result(ctx, s, s.entityPage(q, kinds...), o)
}

// EntityByName finds a merged genre, studio, or person by name.
func (c *Catalog) EntityByName(ctx context.Context, kind, name string, o DTOOptions) (map[string]any, error) {
	return c.Item(ctx, entityID(kind, name), o)
}

// ArtistByName finds a music artist by name.
func (c *Catalog) ArtistByName(ctx context.Context, name string, o DTOOptions) (map[string]any, error) {
	s := c.snap.Load()
	want := identity.NormTitle(name)
	for _, n := range s.all {
		if n.typ == "MusicArtist" && identity.NormTitle(n.name) == want {
			return c.Item(ctx, n.id, o)
		}
	}
	return nil, nil
}

// Seasons lists a series' seasons.
func (c *Catalog) Seasons(ctx context.Context, seriesID string, o DTOOptions) (*QueryResult, error) {
	s := c.snap.Load()
	q := Query{SeriesID: seriesID, IncludeTypes: []string{"Season"}, SortBy: []string{"IndexNumber"}}
	return c.result(ctx, s, s.run(q), o)
}

// Episodes lists a series' episodes, optionally for one season (by ID or number).
func (c *Catalog) Episodes(ctx context.Context, seriesID, seasonID string, season *int, start, limit int, o DTOOptions) (*QueryResult, error) {
	s := c.snap.Load()
	q := Query{SeriesID: seriesID, SeasonID: seasonID, ParentIndexNumber: season, IncludeTypes: []string{"Episode"},
		SortBy: []string{"ParentIndexNumber", "IndexNumber"}, StartIndex: start, Limit: limit}
	return c.result(ctx, s, s.run(q), o)
}

// Filters returns the legacy /Items/Filters values for items matching q.
func (c *Catalog) Filters(q Query) map[string]any {
	s := c.snap.Load()
	q.StartIndex, q.Limit = 0, 0
	genres, tags, ratings, years := map[string]bool{}, map[string]bool{}, map[string]bool{}, map[int]bool{}
	for _, id := range s.run(q).IDs {
		n := s.nodes[id]
		for _, g := range n.genres {
			genres[s.entities[g].name] = true
		}
		for _, t := range n.tags {
			tags[t] = true
		}
		if n.officialRating != "" {
			ratings[n.officialRating] = true
		}
		if n.year > 0 {
			years[n.year] = true
		}
	}
	sorted := func(m map[string]bool) []string {
		out := []string{}
		for k := range m {
			out = append(out, k)
		}
		slices.SortFunc(out, func(a, b string) int { return strings.Compare(strings.ToLower(a), strings.ToLower(b)) })
		return out
	}
	ys := []int{}
	for y := range years {
		ys = append(ys, y)
	}
	slices.Sort(ys)
	return map[string]any{"Genres": sorted(genres), "Tags": sorted(tags), "OfficialRatings": sorted(ratings), "Years": ys}
}

// Filters2 returns the /Items/Filters2 genres (with IDs) and tags.
func (c *Catalog) Filters2(q Query) map[string]any {
	s := c.snap.Load()
	q.StartIndex, q.Limit = 0, 0
	seen := map[string]bool{}
	var genres []map[string]string
	tags := map[string]bool{}
	for _, id := range s.run(q).IDs {
		n := s.nodes[id]
		for _, g := range n.genres {
			if !seen[g] {
				seen[g] = true
				genres = append(genres, map[string]string{"Name": s.entities[g].name, "Id": g})
			}
		}
		for _, t := range n.tags {
			tags[t] = true
		}
	}
	slices.SortFunc(genres, func(a, b map[string]string) int {
		return strings.Compare(strings.ToLower(a["Name"]), strings.ToLower(b["Name"]))
	})
	tagList := []string{}
	for t := range tags {
		tagList = append(tagList, t)
	}
	slices.Sort(tagList)
	if genres == nil {
		genres = []map[string]string{}
	}
	return map[string]any{"Genres": genres, "Tags": tagList}
}

// SearchHints answers /Search/Hints.
func (c *Catalog) SearchHints(ctx context.Context, q Query, o DTOOptions) (map[string]any, error) {
	s := c.snap.Load()
	r := s.run(q)
	items, err := c.dtos(ctx, s, r.IDs, o)
	if err != nil {
		return nil, err
	}
	hints := []map[string]any{}
	for _, d := range items {
		h := map[string]any{"ItemId": d["Id"], "Id": d["Id"], "Name": d["Name"], "MatchedTerm": d["Name"]}
		for _, k := range []string{"Type", "MediaType", "IsFolder", "ProductionYear", "RunTimeTicks", "IndexNumber",
			"ParentIndexNumber", "Album", "AlbumId", "AlbumArtist", "Artists", "SeriesName", "PrimaryImageAspectRatio",
			"StartDate", "EndDate", "ChannelId", "ChannelName", "Status", "SongCount", "EpisodeCount"} {
			if v, ok := d[k]; ok {
				h[k] = v
			}
		}
		if sn, ok := d["SeriesName"]; ok {
			h["Series"] = sn
		}
		if tags, ok := d["ImageTags"].(map[string]any); ok {
			if t, ok := tags["Primary"]; ok {
				h["PrimaryImageTag"] = t
			}
			if t, ok := tags["Thumb"]; ok {
				h["ThumbImageTag"] = t
				h["ThumbImageItemId"] = d["Id"]
			}
		}
		if bd, ok := d["BackdropImageTags"].([]any); ok && len(bd) > 0 {
			h["BackdropImageTag"] = bd[0]
			h["BackdropImageItemId"] = d["Id"]
		}
		hints = append(hints, h)
	}
	return map[string]any{"SearchHints": hints, "TotalRecordCount": r.Total}, nil
}

// optionalFields maps Jellyfin ItemFields to the DTO keys they control. List
// responses include only requested optional fields; ParentId is always kept.
var optionalFields = map[string][]string{
	"AirTime": {"AirTime", "AirDays"}, "Chapters": {"Chapters"}, "ChildCount": {"ChildCount"},
	"CumulativeRunTimeTicks": {"CumulativeRunTimeTicks"}, "DateCreated": {"DateCreated"},
	"DateLastMediaAdded": {"DateLastMediaAdded"}, "Etag": {"Etag"}, "ExternalUrls": {"ExternalUrls"},
	"Genres": {"Genres", "GenreItems"}, "Height": {"Height"}, "Width": {"Width"}, "MediaSources": {"MediaSources"},
	"MediaStreams": {"MediaStreams"}, "OriginalTitle": {"OriginalTitle"}, "Overview": {"Overview"},
	"Path": {"Path"}, "People": {"People"}, "PrimaryImageAspectRatio": {"PrimaryImageAspectRatio"},
	"ProductionLocations": {"ProductionLocations"}, "ProviderIds": {"ProviderIds"},
	"RecursiveItemCount": {"RecursiveItemCount"}, "RemoteTrailers": {"RemoteTrailers"},
	"SeriesPrimaryImage": {"SeriesPrimaryImageTag"}, "SeriesStudio": {"SeriesStudio"}, "SortName": {"SortName"},
	"SpecialFeatureCount": {"SpecialFeatureCount"}, "Studios": {"Studios"}, "Taglines": {"Taglines"}, "Tags": {"Tags"},
}

func trim(d map[string]any, fields []string) {
	if fields == nil {
		return
	}
	want := map[string]bool{}
	for _, f := range fields {
		want[strings.ToLower(strings.TrimSpace(f))] = true
	}
	for f, keys := range optionalFields {
		if !want[strings.ToLower(f)] {
			for _, k := range keys {
				delete(d, k)
			}
		}
	}
}

func userDataStub(id string) map[string]any {
	return map[string]any{"PlaybackPositionTicks": 0, "PlayCount": 0, "IsFavorite": false, "Played": false, "Key": id, "ItemId": id}
}

func isEmpty(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case string:
		return t == ""
	case []any:
		return len(t) == 0
	case map[string]any:
		return len(t) == 0
	}
	return false
}

// rewriter maps one upstream's IDs in a DTO to JellyBloom's.
func (c *Catalog) rewriter(ctx context.Context, s *snapshot, up, userID string, memo map[ref]string) *rewrite.Replacer {
	upUser, upServer := c.o.UpstreamIDs(ctx, up)
	upUser, upServer = identity.NormalizeID(upUser), identity.NormalizeID(upServer)
	return &rewrite.Replacer{GUID: func(n string) (string, bool) {
		r := ref{up, n}
		if u := s.byRef[r]; u != "" {
			return u, true
		}
		if a := s.alias[r]; a != "" {
			return a, true
		}
		switch {
		case n == upUser && upUser != "" && userID != "":
			return userID, true
		case n == upServer && upServer != "":
			return c.o.ServerID, true
		}
		if u, ok := memo[r]; ok {
			return u, true
		}
		u, err := c.o.Resolver.UnifiedID(ctx, up, n)
		if err != nil {
			return "", false
		}
		memo[r] = u
		return u, true
	}, Paths: true}
}

func decode(raw []byte) map[string]any {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var m map[string]any
	if dec.Decode(&m) != nil {
		return nil
	}
	return m
}

// loadDTOs fetches raw DTOs for upstream copies, batched per upstream.
func (c *Catalog) loadDTOs(ctx context.Context, refs []ref) (map[ref][]byte, error) {
	out := map[ref][]byte{}
	byUp := map[string][]string{}
	for _, r := range refs {
		byUp[r.up] = append(byUp[r.up], r.id)
	}
	for up, ids := range byUp {
		for chunk := range slices.Chunk(ids, 400) {
			args := []any{up}
			for _, id := range chunk {
				args = append(args, id)
			}
			rows, err := c.o.DB.QueryContext(ctx, `SELECT real_id, dto FROM items WHERE upstream = ? AND real_id IN (?`+
				strings.Repeat(",?", len(chunk)-1)+`)`, args...)
			if err != nil {
				return nil, err
			}
			for rows.Next() {
				var id string
				var dto []byte
				if err := rows.Scan(&id, &dto); err != nil {
					rows.Close()
					return nil, err
				}
				out[ref{up, id}] = dto
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

// dtos builds Jellyfin DTOs for unified IDs, skipping unknown ones.
func (c *Catalog) dtos(ctx context.Context, s *snapshot, ids []string, o DTOOptions) ([]map[string]any, error) {
	var refs []ref
	for _, id := range ids {
		if n := s.nodes[id]; n != nil {
			refs = append(refs, n.copies...)
		}
	}
	raws, err := c.loadDTOs(ctx, refs)
	if err != nil {
		return nil, err
	}
	memo := map[ref]string{}
	reps := map[string]*rewrite.Replacer{}
	rep := func(up string) *rewrite.Replacer {
		if reps[up] == nil {
			reps[up] = c.rewriter(ctx, s, up, o.UserID, memo)
		}
		return reps[up]
	}

	uds, err := c.userDataDTOs(ctx, s, o.UserID, ids)
	if err != nil {
		return nil, err
	}

	var out []map[string]any
	for _, id := range ids {
		var d map[string]any
		switch {
		case s.nodes[id] != nil:
			d = c.nodeDTO(s, s.nodes[id], raws, rep)
		case s.viewByID[id] != nil:
			d, err = c.viewDTO(ctx, s, s.viewByID[id], rep)
			if err != nil {
				return nil, err
			}
		case s.entities[id] != nil:
			d = c.entityDTO(s.entities[id])
		}
		if d == nil {
			continue
		}
		d["ServerId"] = c.o.ServerID
		d["UserData"] = uds[id]
		trim(d, o.Fields)
		out = append(out, d)
	}
	return out, nil
}

// nodeDTO merges an item's copies: the preferred upstream's DTO, with empty
// fields filled from the other copies.
func (c *Catalog) nodeDTO(s *snapshot, n *node, raws map[ref][]byte, rep func(string) *rewrite.Replacer) map[string]any {
	var d map[string]any
	for _, r := range n.copies {
		m := decode(raws[r])
		if m == nil {
			continue
		}
		m = rep(r.up).Value(m).(map[string]any)
		if d == nil {
			d = m
			continue
		}
		for k, v := range m {
			if isEmpty(d[k]) && !isEmpty(v) {
				d[k] = v
			}
		}
	}
	if d == nil {
		return nil
	}
	d["Id"] = n.id
	if n.folder {
		d["ChildCount"] = len(s.children[n.id])
		leaves := 0
		for _, x := range s.descendants(n.id) {
			if !x.folder {
				leaves++
			}
		}
		d["RecursiveItemCount"] = leaves
	}
	return d
}

func (c *Catalog) viewDTO(ctx context.Context, s *snapshot, v *view, rep func(string) *rewrite.Replacer) (map[string]any, error) {
	var raw []byte
	err := c.o.DB.QueryRowContext(ctx, `SELECT dto FROM views WHERE upstream = ? AND real_id = ? COLLATE NOCASE`,
		v.refs[0].up, v.refs[0].id).Scan(&raw)
	d := map[string]any{}
	if err == nil {
		if m := decode(raw); m != nil {
			d = rep(v.refs[0].up).Value(m).(map[string]any)
		}
	} else if err != sql.ErrNoRows {
		return nil, err
	}
	top := 0
	for _, n := range s.all {
		if s.topLevel(n, v.id) {
			top++
		}
	}
	delete(d, "Path")
	delete(d, "ParentId")
	d["Id"] = v.id
	d["Name"] = v.name
	d["CollectionType"] = v.collectionType
	d["Type"] = "CollectionFolder"
	d["IsFolder"] = true
	d["ChildCount"] = top
	if _, ok := d["SortName"]; ok {
		d["SortName"] = strings.ToLower(v.name)
	}
	return d, nil
}

func (c *Catalog) entityDTO(e *entity) map[string]any {
	d := map[string]any{
		"Name": e.name, "Id": e.id, "Type": e.typ, "IsFolder": false, "ImageTags": map[string]any{},
		"BackdropImageTags": []any{}, "ImageBlurHashes": map[string]any{}, "LocationType": "FileSystem",
		"MediaType": "Unknown", "ChildCount": 0,
	}
	if e.imageTag != "" {
		d["ImageTags"] = map[string]any{"Primary": e.imageTag}
	}
	if e.typ == "Person" {
		d["PrimaryImageTag"] = e.imageTag
	}
	return d
}
