package catalog

import (
	"context"
	"encoding/json"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/NebulousOinkler/JellyBloom/internal/identity"
	"github.com/NebulousOinkler/JellyBloom/internal/index"
	"github.com/NebulousOinkler/JellyBloom/internal/userdata"
)

type dto = map[string]any

// g makes a recognizable 32-hex ID.
func g(s string) string {
	b := []byte("00000000000000000000000000000000")
	copy(b, []byte(s))
	for i, c := range b[:len(s)] {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			b[i] = 'f'
		}
	}
	return string(b)
}

var (
	// upstream a: video
	vMa, vSa                     = g("a1"), g("a2")
	m1a, m2a                     = g("a3"), g("a4")
	sa, s1a, e1a, e2a            = g("a5"), g("a6"), g("a7"), g("a8")
	genreSciA, genreComA, ridley = g("a9"), g("aa"), g("ab")
	studioFox                    = g("ac")
	userA, serverA               = g("ad"), g("ae")
	// upstream b: music, plus an overlapping Movies library and Kids Movies
	vMb, vKb, vMub     = g("b1"), g("b2"), g("b3")
	m1b, k1b           = g("b4"), g("b5")
	artist, album, trk = g("b6"), g("b7"), g("b8")
	genreSciB          = g("b9")
)

type fixture struct {
	t   *testing.T
	idx *index.Store
	res *identity.Resolver
	cat *Catalog
	ud  *userdata.Store
}

func put(t testing.TB, idx *index.Store, up string, views []dto, byView map[string][]dto) {
	t.Helper()
	ctx := context.Background()
	fs, err := idx.BeginFullSync(ctx, up)
	if err != nil {
		t.Fatal(err)
	}
	raw := func(ds []dto) []json.RawMessage {
		var out []json.RawMessage
		for _, d := range ds {
			b, _ := json.Marshal(d)
			out = append(out, b)
		}
		return out
	}
	if err := fs.PutViews(ctx, raw(views)); err != nil {
		t.Fatal(err)
	}
	for v, items := range byView {
		if err := fs.PutItems(ctx, v, raw(items)); err != nil {
			t.Fatal(err)
		}
	}
	if err := fs.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

func newFixture(t *testing.T, mode identity.Mode) *fixture {
	ctx := context.Background()
	idx, err := index.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { idx.Close() })

	put(t, idx, "a",
		[]dto{{"Id": vMa, "Name": "Movies", "CollectionType": "movies", "ImageTags": dto{"Primary": "tagMa"}}, {"Id": vSa, "Name": "Shows", "CollectionType": "tvshows"}},
		map[string][]dto{
			vMa: {
				{"Id": m1a, "Type": "Movie", "Name": "Alien", "ProductionYear": 1979, "DateCreated": "2020-01-01T00:00:00Z", "ParentId": vMa,
					"ProviderIds": dto{"Tmdb": "1"}, "GenreItems": []dto{{"Name": "Sci-Fi", "Id": genreSciA}},
					"People":  []dto{{"Name": "Ridley Scott", "Id": ridley, "Type": "Director", "PrimaryImageTag": "tagRidley"}},
					"Studios": []dto{{"Name": "Fox", "Id": studioFox}}, "UserData": dto{"Played": true}, "MediaType": "Video"},
				{"Id": m2a, "Type": "Movie", "Name": "Brazil", "ProductionYear": 1985, "DateCreated": "2021-01-01T00:00:00Z", "ParentId": vMa,
					"ProviderIds": dto{"Tmdb": "2"}, "GenreItems": []dto{{"Name": "Comedy", "Id": genreComA}}, "Overview": "Dystopia", "MediaType": "Video"},
			},
			vSa: {
				{"Id": sa, "Type": "Series", "Name": "Show", "IsFolder": true, "ParentId": vSa, "ProviderIds": dto{"Tvdb": "9"}, "DateCreated": "2019-01-01T00:00:00Z"},
				{"Id": s1a, "Type": "Season", "Name": "Season 1", "IsFolder": true, "ParentId": sa, "SeriesId": sa, "IndexNumber": 1},
				{"Id": e2a, "Type": "Episode", "Name": "Two", "ParentId": s1a, "SeriesId": sa, "SeasonId": s1a, "ParentIndexNumber": 1, "IndexNumber": 2, "DateCreated": "2022-02-01T00:00:00Z"},
				{"Id": e1a, "Type": "Episode", "Name": "One", "ParentId": s1a, "SeriesId": sa, "SeasonId": s1a, "ParentIndexNumber": 1, "IndexNumber": 1, "DateCreated": "2022-01-01T00:00:00Z"},
			},
		})
	put(t, idx, "b",
		[]dto{{"Id": vMb, "Name": "MOVIES", "CollectionType": "movies"}, {"Id": vKb, "Name": "Kids Movies", "CollectionType": "movies"},
			{"Id": vMub, "Name": "Music", "CollectionType": "music"}},
		map[string][]dto{
			vMb: {{"Id": m1b, "Type": "Movie", "Name": "Alien", "ProductionYear": 1979, "ParentId": vMb, "ProviderIds": dto{"Tmdb": "1"},
				"Overview": "In space", "GenreItems": []dto{{"Name": "sci-fi", "Id": genreSciB}}, "MediaType": "Video"}},
			vKb: {{"Id": k1b, "Type": "Movie", "Name": "Cars", "ProductionYear": 2006, "ParentId": vKb, "ProviderIds": dto{"Tmdb": "3"}, "MediaType": "Video"}},
			vMub: {
				{"Id": artist, "Type": "MusicArtist", "Name": "Björk", "IsFolder": true},
				{"Id": album, "Type": "MusicAlbum", "Name": "Homogenic", "IsFolder": true, "ParentId": vMub, "AlbumArtist": "Björk", "AlbumArtists": []dto{{"Name": "Björk", "Id": artist}}},
				{"Id": trk, "Type": "Audio", "Name": "Hunter", "ParentId": album, "AlbumId": album, "IndexNumber": 1, "ArtistItems": []dto{{"Name": "Björk", "Id": artist}}, "MediaType": "Audio"},
			},
		})

	mode.Upstreams = []string{"a", "b"}
	if mode.Op == "" {
		mode.Op = identity.Union
	}
	res, err := identity.New(idx, mode, nil)
	if err != nil {
		t.Fatal(err)
	}
	ud, err := userdata.Open(idx.DB())
	if err != nil {
		t.Fatal(err)
	}
	cat := New(Options{DB: idx.DB(), Resolver: res, Upstreams: []string{"a", "b"}, ServerID: "jbserver", UserData: ud,
		UpstreamIDs: func(_ context.Context, up string) (string, string) {
			if up == "a" {
				return userA, serverA
			}
			return "", ""
		}})
	res.OnRebuild(func(ctx context.Context) {
		if err := cat.Reload(ctx); err != nil {
			t.Fatal(err)
		}
	})
	if _, err := res.Rebuild(ctx); err != nil {
		t.Fatal(err)
	}
	return &fixture{t: t, idx: idx, res: res, cat: cat, ud: ud}
}

func (f *fixture) uid(up, real string) string {
	f.t.Helper()
	id, err := f.res.UnifiedID(context.Background(), up, real)
	if err != nil {
		f.t.Fatal(err)
	}
	return id
}

func names(items []map[string]any) []string {
	var out []string
	for _, it := range items {
		out = append(out, it["Name"].(string))
	}
	return out
}

func (f *fixture) query(q Query, fields ...string) *QueryResult {
	f.t.Helper()
	o := DTOOptions{UserID: "jbuser"}
	if fields != nil {
		o.Fields = fields
	}
	r, err := f.cat.Query(context.Background(), q, o)
	if err != nil {
		f.t.Fatal(err)
	}
	return r
}

func (f *fixture) viewID(name string) string {
	f.t.Helper()
	views, _ := f.cat.Views(context.Background(), DTOOptions{})
	for _, v := range views.Items {
		if v["Name"] == name {
			return v["Id"].(string)
		}
	}
	f.t.Fatalf("no view %q in %v", name, names(views.Items))
	return ""
}

func TestViewsMergeByTypeAndName(t *testing.T) {
	f := newFixture(t, identity.Mode{})
	views, err := f.cat.Views(context.Background(), DTOOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := names(views.Items); !slices.Equal(got, []string{"Movies", "Shows", "Kids Movies", "Music"}) {
		t.Errorf("views = %v", got)
	}
	movies := views.Items[0]
	if movies["CollectionType"] != "movies" || movies["Type"] != "CollectionFolder" || movies["ServerId"] != "jbserver" {
		t.Errorf("movies view = %v", movies)
	}
	if movies["ChildCount"] != 2 {
		t.Errorf("movies ChildCount = %v, want 2 (Alien deduplicated)", movies["ChildCount"])
	}

	// The merged library's ID routes to the preferred upstream's library and
	// is what both upstreams' library IDs map to.
	u, err := f.res.Lookup(context.Background(), movies["Id"].(string))
	if err != nil || u.Refs[0].Upstream != "a" || u.Refs[0].ID != vMa || len(u.Refs) != 2 {
		t.Errorf("view alias = %+v, %v", u, err)
	}
	if f.uid("b", vMb) != movies["Id"] {
		t.Error("b's Movies library does not map to the merged library")
	}
}

func TestQueryByLibrary(t *testing.T) {
	f := newFixture(t, identity.Mode{})
	movies := f.viewID("Movies")

	r := f.query(Query{ParentID: movies, Recursive: true, IncludeTypes: []string{"Movie"}, SortBy: []string{"SortName"}})
	if got := names(r.Items); !slices.Equal(got, []string{"Alien", "Brazil"}) || r.TotalRecordCount != 2 {
		t.Errorf("movies = %v (total %d)", got, r.TotalRecordCount)
	}
	if got := names(f.query(Query{ParentID: movies}).Items); !slices.Equal(got, []string{"Alien", "Brazil"}) {
		t.Errorf("top level of Movies = %v", got)
	}
	if got := names(f.query(Query{ParentID: f.viewID("Kids Movies"), Recursive: true}).Items); !slices.Equal(got, []string{"Cars"}) {
		t.Errorf("Kids Movies = %v", got)
	}

	// Paging and descending sort.
	r = f.query(Query{ParentID: movies, Recursive: true, SortBy: []string{"ProductionYear"}, SortOrder: []string{"Descending"}, StartIndex: 1, Limit: 1})
	if got := names(r.Items); !slices.Equal(got, []string{"Alien"}) || r.TotalRecordCount != 2 || r.StartIndex != 1 {
		t.Errorf("page = %v total %d start %d", got, r.TotalRecordCount, r.StartIndex)
	}

	// Search folds case and accents; filters work.
	if got := names(f.query(Query{SearchTerm: "ALIE", Recursive: true}).Items); !slices.Equal(got, []string{"Alien"}) {
		t.Errorf("search = %v", got)
	}
	if got := names(f.query(Query{SearchTerm: "bjork", Recursive: true}).Items); !slices.Equal(got, []string{"Björk"}) {
		t.Errorf("accent-folded search = %v", got)
	}
	if got := names(f.query(Query{Recursive: true, Years: []int{1985}}).Items); !slices.Equal(got, []string{"Brazil"}) {
		t.Errorf("years = %v", got)
	}
	if got := f.query(Query{Recursive: true, Filters: []string{"IsFavorite"}}).TotalRecordCount; got != 0 {
		t.Errorf("IsFavorite without user data = %d", got)
	}
}

func TestShows(t *testing.T) {
	f := newFixture(t, identity.Mode{})
	series := f.uid("a", sa)
	if got := names(f.query(Query{ParentID: f.viewID("Shows")}).Items); !slices.Equal(got, []string{"Show"}) {
		t.Errorf("Shows top level = %v", got)
	}
	seasons, _ := f.cat.Seasons(context.Background(), series, DTOOptions{})
	if got := names(seasons.Items); !slices.Equal(got, []string{"Season 1"}) {
		t.Errorf("seasons = %v", got)
	}
	eps, _ := f.cat.Episodes(context.Background(), series, "", nil, 0, 0, DTOOptions{})
	if got := names(eps.Items); !slices.Equal(got, []string{"One", "Two"}) {
		t.Errorf("episodes = %v (want season/episode order)", got)
	}
	s := seasons.Items[0]
	if s["ChildCount"] != 2 || s["RecursiveItemCount"] != 2 {
		t.Errorf("season counts = %v %v", s["ChildCount"], s["RecursiveItemCount"])
	}
	show, _ := f.cat.Item(context.Background(), series, DTOOptions{})
	if show["RecursiveItemCount"] != 2 {
		t.Errorf("series RecursiveItemCount = %v", show["RecursiveItemCount"])
	}
}

func TestMergedItemDTO(t *testing.T) {
	f := newFixture(t, identity.Mode{})
	alien := f.uid("a", m1a)
	d, err := f.cat.Item(context.Background(), alien, DTOOptions{UserID: "jbuser"})
	if err != nil || d == nil {
		t.Fatal(err)
	}
	if d["Id"] != alien || d["ServerId"] != "jbserver" {
		t.Errorf("Id/ServerId = %v %v", d["Id"], d["ServerId"])
	}
	if d["Overview"] != "In space" {
		t.Errorf("Overview = %v, want filled from b", d["Overview"])
	}
	if d["ParentId"] != f.viewID("Movies") {
		t.Errorf("ParentId = %v, want the merged Movies library", d["ParentId"])
	}
	genre := d["GenreItems"].([]any)[0].(map[string]any)
	if genre["Id"] != entityID("Genre", "Sci-Fi") {
		t.Errorf("genre id = %v", genre["Id"])
	}
	person := d["People"].([]any)[0].(map[string]any)
	if person["Id"] != entityID("Person", "Ridley Scott") || person["PrimaryImageTag"] != "tagRidley" {
		t.Errorf("person = %v", person)
	}
	if ud := d["UserData"].(map[string]any); ud["Played"] != false || ud["ItemId"] != alien {
		t.Errorf("upstream UserData leaked: %v", ud)
	}
}

func TestFieldsTrimming(t *testing.T) {
	f := newFixture(t, identity.Mode{})
	r := f.query(Query{IDs: []string{f.uid("a", m2a)}}, "Overview")
	d := r.Items[0]
	if d["Overview"] != "Dystopia" {
		t.Error("requested field missing")
	}
	for _, k := range []string{"GenreItems", "ProviderIds", "DateCreated"} {
		if _, ok := d[k]; ok {
			t.Errorf("unrequested optional field %s present", k)
		}
	}
	if _, ok := d["ParentId"]; !ok {
		t.Error("ParentId must always be present")
	}
}

func TestLatest(t *testing.T) {
	f := newFixture(t, identity.Mode{})
	ctx := context.Background()
	got, _ := f.cat.Latest(ctx, f.viewID("Movies"), nil, true, false, 10, DTOOptions{})
	if n := names(got); !slices.Equal(n, []string{"Brazil", "Alien"}) {
		t.Errorf("latest movies = %v", n)
	}
	got, _ = f.cat.Latest(ctx, f.viewID("Shows"), nil, true, false, 10, DTOOptions{})
	if n := names(got); !slices.Equal(n, []string{"Show"}) {
		t.Errorf("latest shows (grouped) = %v", n)
	}
	got, _ = f.cat.Latest(ctx, f.viewID("Shows"), nil, false, false, 10, DTOOptions{})
	if n := names(got); !slices.Equal(n, []string{"Two", "One"}) {
		t.Errorf("latest shows (ungrouped) = %v", n)
	}
}

func TestEntitiesAndArtists(t *testing.T) {
	f := newFixture(t, identity.Mode{})
	ctx := context.Background()
	movies := f.viewID("Movies")

	genres, _ := f.cat.Entities(ctx, Query{ParentID: movies, Recursive: true}, DTOOptions{}, "Genre")
	if got := names(genres.Items); !slices.Equal(got, []string{"Comedy", "Sci-Fi"}) {
		t.Errorf("genres = %v (Sci-Fi and sci-fi must merge)", got)
	}
	people, _ := f.cat.Entities(ctx, Query{Recursive: true}, DTOOptions{}, "Person")
	if p := people.Items[0]; p["Name"] != "Ridley Scott" || p["ImageTags"].(map[string]any)["Primary"] != "tagRidley" {
		t.Errorf("people = %v", people.Items)
	}
	sci := entityID("Genre", "Sci-Fi")
	if got := names(f.query(Query{Recursive: true, GenreIDs: []string{sci}}).Items); !slices.Equal(got, []string{"Alien"}) {
		t.Errorf("genre filter = %v", got)
	}
	if got := names(f.query(Query{ParentID: sci}).Items); !slices.Equal(got, []string{"Alien"}) {
		t.Errorf("parentId=genre = %v", got)
	}
	// Genre images route to an upstream genre object.
	if u, err := f.res.Lookup(ctx, sci); err != nil || u.Refs[0].ID != genreSciA || len(u.Refs) != 2 {
		t.Errorf("genre alias = %+v %v", u, err)
	}

	music := f.viewID("Music")
	for _, albumArtists := range []bool{false, true} {
		a, _ := f.cat.Artists(ctx, Query{ParentID: music, Recursive: true}, albumArtists, DTOOptions{})
		if got := names(a.Items); !slices.Equal(got, []string{"Björk"}) {
			t.Errorf("artists(albumArtists=%v) = %v", albumArtists, got)
		}
	}
	a, _ := f.cat.Artists(ctx, Query{Recursive: true, SearchTerm: "zzz"}, false, DTOOptions{})
	if a.TotalRecordCount != 0 {
		t.Error("artist search must match artist names")
	}
	if d, _ := f.cat.ArtistByName(ctx, "bjork", DTOOptions{}); d == nil || d["Name"] != "Björk" {
		t.Errorf("ArtistByName = %v", d)
	}
}

func TestFilters(t *testing.T) {
	f := newFixture(t, identity.Mode{})
	got := f.cat.Filters(Query{ParentID: f.viewID("Movies"), Recursive: true})
	if g := got["Genres"].([]string); !slices.Equal(g, []string{"Comedy", "Sci-Fi"}) {
		t.Errorf("genres = %v", g)
	}
	if y := got["Years"].([]int); !slices.Equal(y, []int{1979, 1985}) {
		t.Errorf("years = %v", y)
	}
}

func TestSearchHints(t *testing.T) {
	f := newFixture(t, identity.Mode{})
	h, err := f.cat.SearchHints(context.Background(), Query{SearchTerm: "hunt", Recursive: true}, DTOOptions{})
	if err != nil {
		t.Fatal(err)
	}
	hints := h["SearchHints"].([]map[string]any)
	if len(hints) != 1 || hints[0]["Name"] != "Hunter" || hints[0]["Type"] != "Audio" || hints[0]["ItemId"] != f.uid("b", trk) {
		t.Errorf("hints = %v", hints)
	}
}

func TestDifferenceHidesItemsAndLibraries(t *testing.T) {
	f := newFixture(t, identity.Mode{Op: identity.Difference, Base: "a", Subtract: []string{"b"}})
	if got := names(f.query(Query{Recursive: true, IncludeTypes: []string{"Movie"}}).Items); !slices.Equal(got, []string{"Brazil"}) {
		t.Errorf("a \\ b movies = %v", got)
	}
	if !f.cat.Hidden(f.uid("b", k1b)) || !f.cat.Hidden(f.uid("a", m1a)) || f.cat.Hidden(f.uid("a", m2a)) {
		t.Error("Hidden wrong")
	}
	if f.cat.Hidden(g("ff")) {
		t.Error("unknown IDs are not hidden")
	}
	views, _ := f.cat.Views(context.Background(), DTOOptions{})
	if got := names(views.Items); !slices.Equal(got, []string{"Movies", "Shows"}) {
		t.Errorf("views under a \\ b = %v, want only libraries with visible items", got)
	}
}

// TestRootFoldersAreTransparent mirrors how Jellyfin really links library
// contents: items sit under a physical root Folder whose parent is not an
// indexed item. Jellyfin shows the folder's contents directly in the library.
func TestRootFoldersAreTransparent(t *testing.T) {
	ctx := context.Background()
	idx, err := index.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { idx.Close() })
	lib, root, folder, movie := g("c1"), g("c2"), g("c3"), g("c4")
	put(t, idx, "a", []dto{{"Id": lib, "Name": "Movies", "CollectionType": "movies"}}, map[string][]dto{lib: {
		{"Id": folder, "Type": "Folder", "Name": "movies", "IsFolder": true, "ParentId": root},
		{"Id": movie, "Type": "Movie", "Name": "Heat", "ParentId": folder, "ProviderIds": dto{"Tmdb": "949"}},
	}})
	res, _ := identity.New(idx, identity.Mode{Op: identity.Union, Upstreams: []string{"a"}}, nil)
	cat := New(Options{DB: idx.DB(), Resolver: res, Upstreams: []string{"a"}, ServerID: "s"})
	res.OnRebuild(func(ctx context.Context) { cat.Reload(ctx) })
	if _, err := res.Rebuild(ctx); err != nil {
		t.Fatal(err)
	}
	views, _ := cat.Views(ctx, DTOOptions{})
	libID := views.Items[0]["Id"].(string)
	top, _ := cat.Query(ctx, Query{ParentID: libID}, DTOOptions{})
	if got := names(top.Items); !slices.Equal(got, []string{"Heat"}) {
		t.Errorf("library top level = %v, want the folder's contents", got)
	}
	all, _ := cat.Query(ctx, Query{ParentID: libID, Recursive: true}, DTOOptions{})
	if got := names(all.Items); !slices.Equal(got, []string{"Heat"}) {
		t.Errorf("recursive listing = %v, want no root folder", got)
	}
	m, _ := cat.Query(ctx, Query{ParentID: libID}, DTOOptions{Fields: []string{"ParentId"}})
	if m.Items[0]["ParentId"] != libID {
		t.Errorf("movie ParentId = %v, want the library %s", m.Items[0]["ParentId"], libID)
	}
	if views.Items[0]["ChildCount"] != 1 {
		t.Errorf("library ChildCount = %v", views.Items[0]["ChildCount"])
	}
}

func (f *fixture) set(user, up, real string, fn func(*userdata.Record)) {
	f.t.Helper()
	if _, err := f.ud.Update(context.Background(), user, []string{f.uid(up, real)}, fn); err != nil {
		f.t.Fatal(err)
	}
}

func TestUserDataOverlayAndFilters(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, identity.Mode{})
	o := DTOOptions{UserID: "u1"}
	f.set("u1", "a", e1a, func(r *userdata.Record) { userdata.MarkPlayed(r, time.UnixMilli(2000)) })
	f.set("u1", "a", m1a, func(r *userdata.Record) { r.IsFavorite = true })
	f.set("u1", "a", m2a, func(r *userdata.Record) { r.Position, r.LastPlayed = 50, time.UnixMilli(3000) })
	f.set("u2", "a", e2a, func(r *userdata.Record) { userdata.MarkPlayed(r, time.UnixMilli(1)) }) // another user

	series, _ := f.cat.Item(ctx, f.uid("a", sa), o)
	ud := series["UserData"].(map[string]any)
	if ud["UnplayedItemCount"] != 1 || ud["Played"] != false || ud["PlayedPercentage"] != 50.0 {
		t.Errorf("series user data = %v", ud)
	}
	ep, _ := f.cat.Item(ctx, f.uid("a", e1a), o)
	if ud := ep["UserData"].(map[string]any); ud["Played"] != true || ud["PlayCount"] != 1 {
		t.Errorf("episode user data = %v", ud)
	}
	if other, _ := f.cat.Item(ctx, f.uid("a", e2a), o); other["UserData"].(map[string]any)["Played"] != false {
		t.Error("another user's played state leaked")
	}

	yes, no := true, false
	q := func(qq Query) []string {
		r, err := f.cat.Query(ctx, qq, o)
		if err != nil {
			t.Fatal(err)
		}
		return names(r.Items)
	}
	if got := q(Query{Recursive: true, IncludeTypes: []string{"Episode"}, IsPlayed: &yes}); !slices.Equal(got, []string{"One"}) {
		t.Errorf("isPlayed = %v", got)
	}
	if got := q(Query{Recursive: true, IncludeTypes: []string{"Episode"}, Filters: []string{"IsUnplayed"}}); !slices.Equal(got, []string{"Two"}) {
		t.Errorf("IsUnplayed = %v", got)
	}
	if got := q(Query{Recursive: true, IsFavorite: &yes}); !slices.Equal(got, []string{"Alien"}) {
		t.Errorf("isFavorite = %v", got)
	}
	if got := q(Query{Recursive: true, IncludeTypes: []string{"Movie"}, Filters: []string{"IsResumable"}}); !slices.Equal(got, []string{"Brazil"}) {
		t.Errorf("IsResumable = %v", got)
	}
	if got := q(Query{Recursive: true, IncludeTypes: []string{"Series"}, IsPlayed: &no}); !slices.Equal(got, []string{"Show"}) {
		t.Errorf("series isPlayed=false = %v", got)
	}
	got := q(Query{Recursive: true, IncludeTypes: []string{"Movie", "Episode"}, SortBy: []string{"DatePlayed"}, SortOrder: []string{"Descending"}})
	if got[0] != "Brazil" || got[1] != "One" {
		t.Errorf("DatePlayed desc = %v", got)
	}

	// Marking the remaining episode played completes the series.
	f.set("u1", "a", e2a, func(r *userdata.Record) { userdata.MarkPlayed(r, time.UnixMilli(4000)) })
	series, _ = f.cat.Item(ctx, f.uid("a", sa), o)
	if ud := series["UserData"].(map[string]any); ud["Played"] != true || ud["UnplayedItemCount"] != 0 {
		t.Errorf("completed series = %v", ud)
	}
}

func TestResumeNextUpLatest(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, identity.Mode{})
	o := DTOOptions{UserID: "u1"}
	series := f.uid("a", sa)

	nu, _ := f.cat.NextUp(ctx, NextUpOptions{}, o)
	if nu.TotalRecordCount != 0 {
		t.Errorf("nothing watched: next up = %v", names(nu.Items))
	}
	nu, _ = f.cat.NextUp(ctx, NextUpOptions{SeriesID: series}, o)
	if got := names(nu.Items); !slices.Equal(got, []string{"One"}) {
		t.Errorf("unwatched series with SeriesID = %v, want its first episode", got)
	}

	f.set("u1", "a", e1a, func(r *userdata.Record) { userdata.MarkPlayed(r, time.UnixMilli(2000)) })
	nu, _ = f.cat.NextUp(ctx, NextUpOptions{}, o)
	if got := names(nu.Items); !slices.Equal(got, []string{"Two"}) {
		t.Errorf("next up = %v", got)
	}
	f.set("u1", "a", e2a, func(r *userdata.Record) { r.Position, r.LastPlayed = 10, time.UnixMilli(3000) })
	if nu, _ = f.cat.NextUp(ctx, NextUpOptions{}, o); nu.TotalRecordCount != 1 {
		t.Error("an in-progress episode is next up by default")
	}
	if nu, _ = f.cat.NextUp(ctx, NextUpOptions{}, DTOOptions{UserID: "u1"}); nu.Items[0]["UserData"].(map[string]any)["PlaybackPositionTicks"] != int64(10) {
		t.Errorf("next up carries the resume position: %v", nu.Items[0]["UserData"])
	}
	if nu, _ = f.cat.NextUp(ctx, NextUpOptions{SkipResumable: true}, o); nu.TotalRecordCount != 0 {
		t.Error("SkipResumable must skip the in-progress episode")
	}

	res, _ := f.cat.Resume(ctx, Query{}, o)
	if got := names(res.Items); !slices.Equal(got, []string{"Two"}) {
		t.Errorf("resume = %v", got)
	}
	f.set("u1", "a", m2a, func(r *userdata.Record) { r.Position, r.LastPlayed = 99, time.UnixMilli(9000) })
	res, _ = f.cat.Resume(ctx, Query{}, o)
	if got := names(res.Items); !slices.Equal(got, []string{"Brazil", "Two"}) {
		t.Errorf("resume (most recent first) = %v", got)
	}
	res, _ = f.cat.Resume(ctx, Query{IncludeTypes: []string{"Movie"}}, o)
	if got := names(res.Items); !slices.Equal(got, []string{"Brazil"}) {
		t.Errorf("resume movies = %v", got)
	}

	latest, _ := f.cat.Latest(ctx, f.viewID("Shows"), nil, false, true, 10, o)
	if got := names(latest); !slices.Equal(got, []string{"Two"}) {
		t.Errorf("latest hiding played = %v", got)
	}
}
