package identity

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/NebulousOinkler/JellyBloom/internal/index"
)

type dto = map[string]any

func ids(m map[string]string) dto {
	out := dto{}
	for k, v := range m {
		out[k] = v
	}
	return out
}

func movie(id, name string, year int, prov map[string]string) dto {
	return dto{"Id": id, "Type": "Movie", "Name": name, "ProductionYear": year, "ProviderIds": ids(prov)}
}

func episode(id, series, season string, s, e int) dto {
	return dto{"Id": id, "Type": "Episode", "Name": "Ep", "SeriesId": series, "SeasonId": season, "ParentId": season, "ParentIndexNumber": s, "IndexNumber": e}
}

type fixture struct {
	t   testing.TB
	idx *index.Store
}

func newFixture(t testing.TB) *fixture {
	idx, err := index.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { idx.Close() })
	return &fixture{t: t, idx: idx}
}

// set replaces upstream's indexed items.
func (f *fixture) set(upstream string, items ...dto) {
	f.t.Helper()
	ctx := context.Background()
	fs, err := f.idx.BeginFullSync(ctx, upstream)
	if err != nil {
		f.t.Fatal(err)
	}
	var raws []json.RawMessage
	for _, it := range items {
		b, _ := json.Marshal(it)
		raws = append(raws, b)
	}
	if err := fs.PutItems(ctx, "view", raws); err != nil {
		f.t.Fatal(err)
	}
	if err := fs.Commit(ctx); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) resolver(mode Mode) *Resolver {
	f.t.Helper()
	if mode.Op == "" {
		mode.Op = Union
	}
	if mode.Upstreams == nil {
		mode.Upstreams = []string{"a", "b"}
	}
	r, err := New(f.idx, mode, nil)
	if err != nil {
		f.t.Fatal(err)
	}
	if _, err := r.Rebuild(context.Background()); err != nil {
		f.t.Fatal(err)
	}
	return r
}

func uid(t *testing.T, r *Resolver, upstream, id string) string {
	t.Helper()
	u, err := r.UnifiedID(context.Background(), upstream, id)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func lookup(t *testing.T, r *Resolver, id string) *Unified {
	t.Helper()
	u, err := r.Lookup(context.Background(), id)
	if err != nil {
		t.Fatalf("Lookup(%s): %v", id, err)
	}
	return u
}

func sameItem(t *testing.T, r *Resolver, want bool, a, b [2]string) {
	t.Helper()
	ua, ub := uid(t, r, a[0], a[1]), uid(t, r, b[0], b[1])
	if (ua == ub) != want {
		t.Errorf("%v and %v: same=%v, want %v", a, b, ua == ub, want)
	}
}

var idFormat = regexp.MustCompile(`^[0-9a-f]{12}5[0-9a-f]{3}[89ab][0-9a-f]{15}$`)

func TestNewIDFormat(t *testing.T) {
	id := newID("movie:tmdb:603")
	if !idFormat.MatchString(id) {
		t.Errorf("id %q is not a 32-hex UUIDv5", id)
	}
	if newID("movie:tmdb:603") != id || newID("movie:tmdb:604") == id {
		t.Error("newID not deterministic or not distinct")
	}
}

func TestMovieMatching(t *testing.T) {
	f := newFixture(t)
	f.set("a",
		movie("a1", "The Matrix", 1999, map[string]string{"Tmdb": "603", "Imdb": "tt0133093"}),
		movie("a2", "Solaris", 1972, map[string]string{"Tmdb": "593"}),
		movie("a3", "Heat", 1995, nil),
	)
	f.set("b",
		movie("b1", "Matrix, The", 1999, map[string]string{"imdb": "TT0133093"}), // shares IMDb only
		movie("b2", "Solaris", 1972, map[string]string{"Tmdb": "999"}),           // same title+year, conflicting TMDb
		movie("b3", "HEAT!", 1995, nil),                                          // weak match only
		movie("b4", "Heat", 1986, nil),                                           // different year
	)
	r := f.resolver(Mode{})

	sameItem(t, r, true, [2]string{"a", "a1"}, [2]string{"b", "b1"})
	sameItem(t, r, false, [2]string{"a", "a2"}, [2]string{"b", "b2"})
	sameItem(t, r, true, [2]string{"a", "a3"}, [2]string{"b", "b3"})
	sameItem(t, r, false, [2]string{"a", "a3"}, [2]string{"b", "b4"})

	u := lookup(t, r, uid(t, r, "a", "a1"))
	if u.Type != "Movie" || len(u.Refs) != 2 || !u.Visible {
		t.Errorf("matrix = %+v", u)
	}
}

func TestEpisodesMatchThroughSeries(t *testing.T) {
	f := newFixture(t)
	f.set("a",
		dto{"Id": "as", "Type": "Series", "Name": "Show", "ProviderIds": ids(map[string]string{"Tvdb": "1"})},
		dto{"Id": "as1", "Type": "Season", "Name": "Season 1", "SeriesId": "as", "ParentId": "as", "IndexNumber": 1},
		episode("ae1", "as", "as1", 1, 1),
		episode("ae2", "as", "as1", 1, 2),
	)
	f.set("b",
		dto{"Id": "bs", "Type": "Series", "Name": "Show (US)", "ProviderIds": ids(map[string]string{"Tvdb": "1", "Tmdb": "77"})},
		dto{"Id": "bs1", "Type": "Season", "Name": "S1", "SeriesId": "bs", "ParentId": "bs", "IndexNumber": 1},
		episode("be1", "bs", "bs1", 1, 1),
	)
	r := f.resolver(Mode{})

	sameItem(t, r, true, [2]string{"a", "as"}, [2]string{"b", "bs"})
	sameItem(t, r, true, [2]string{"a", "as1"}, [2]string{"b", "bs1"})
	sameItem(t, r, true, [2]string{"a", "ae1"}, [2]string{"b", "be1"})
	sameItem(t, r, false, [2]string{"a", "ae1"}, [2]string{"a", "ae2"})
}

func TestMusicMatching(t *testing.T) {
	f := newFixture(t)
	f.set("a",
		dto{"Id": "aar", "Type": "MusicArtist", "Name": "Björk", "ProviderIds": ids(map[string]string{"MusicBrainzArtist": "mb-art"})},
		dto{"Id": "aal", "Type": "MusicAlbum", "Name": "Homogenic", "AlbumArtist": "Björk", "AlbumArtists": []dto{{"Id": "aar"}}, "ProviderIds": ids(map[string]string{"MusicBrainzReleaseGroup": "rg1"})},
		dto{"Id": "at1", "Type": "Audio", "Name": "Hunter", "AlbumId": "aal", "ParentId": "aal", "IndexNumber": 1, "ParentIndexNumber": 1},
	)
	f.set("b",
		dto{"Id": "bar", "Type": "MusicArtist", "Name": "bjork"},                                                                  // no MBID, name differs by accent
		dto{"Id": "bal", "Type": "MusicAlbum", "Name": "Homogenic", "AlbumArtist": "Björk", "AlbumArtists": []dto{{"Id": "bar"}}}, // weak match
		dto{"Id": "bt1", "Type": "Audio", "Name": "Hunter (Remaster)", "AlbumId": "bal", "ParentId": "bal", "IndexNumber": 1},     // disc defaults to 1
	)
	r := f.resolver(Mode{})

	sameItem(t, r, true, [2]string{"a", "aar"}, [2]string{"b", "bar"}) // accents folded
	sameItem(t, r, true, [2]string{"a", "aal"}, [2]string{"b", "bal"})
	sameItem(t, r, true, [2]string{"a", "at1"}, [2]string{"b", "bt1"})
}

func TestUnmatchedTypesNeverMerge(t *testing.T) {
	f := newFixture(t)
	f.set("a", dto{"Id": "af", "Type": "Folder", "Name": "Stuff"}, dto{"Id": "ab", "Type": "Book", "Name": "Dune"})
	f.set("b", dto{"Id": "bf", "Type": "Folder", "Name": "Stuff"}, dto{"Id": "bb", "Type": "Book", "Name": "Dune"})
	r := f.resolver(Mode{})
	sameItem(t, r, false, [2]string{"a", "af"}, [2]string{"b", "bf"})
	sameItem(t, r, false, [2]string{"a", "ab"}, [2]string{"b", "bb"})
}

func TestIDsAreStable(t *testing.T) {
	f := newFixture(t)
	matrix := movie("a1", "The Matrix", 1999, map[string]string{"Imdb": "tt0133093"})
	f.set("a", matrix)
	f.set("b")
	r := f.resolver(Mode{})
	id := uid(t, r, "a", "a1")

	// A new copy with an extra, preferred provider ID must not change the ID.
	f.set("b", movie("b1", "The Matrix", 1999, map[string]string{"Tmdb": "603", "Imdb": "tt0133093"}))
	if _, err := r.Rebuild(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := uid(t, r, "b", "b1"); got != id {
		t.Errorf("ID changed when a copy was added: %s -> %s", id, got)
	}

	// Removed everywhere, then re-added under new upstream IDs: same ID.
	f.set("a")
	f.set("b")
	r.Rebuild(context.Background())
	f.set("b", movie("b9", "The Matrix", 1999, map[string]string{"Tmdb": "603"}))
	r.Rebuild(context.Background())
	if got := uid(t, r, "b", "b9"); got != id {
		t.Errorf("ID changed after remove and re-add: %s -> %s", id, got)
	}
}

func TestBridgingCopyRedirectsOldID(t *testing.T) {
	f := newFixture(t)
	f.set("a", movie("a1", "Alien", 0, map[string]string{"Imdb": "tt0078748"}))
	f.set("b", movie("b1", "Alien", 0, map[string]string{"Tmdb": "348"}))
	r := f.resolver(Mode{})
	idA, idB := uid(t, r, "a", "a1"), uid(t, r, "b", "b1")
	if idA == idB {
		t.Fatal("should not match yet: no shared key")
	}

	f.set("b", movie("b1", "Alien", 0, map[string]string{"Tmdb": "348"}), movie("b2", "Alien", 0, map[string]string{"Tmdb": "348", "Imdb": "tt0078748"}))
	if _, err := r.Rebuild(context.Background()); err != nil {
		t.Fatal(err)
	}
	winner := uid(t, r, "a", "a1")
	if uid(t, r, "b", "b1") != winner || uid(t, r, "b", "b2") != winner {
		t.Fatal("bridging copy did not merge the groups")
	}
	for _, old := range []string{idA, idB} {
		if u := lookup(t, r, old); u.ID != winner || len(u.Refs) != 3 {
			t.Errorf("Lookup(%s) = %+v, want redirect to %s", old, u, winner)
		}
	}
}

// library builds two upstreams with overlapping content.
func library(f *fixture) {
	show := func(prefix string, eps ...int) []dto {
		out := []dto{
			{"Id": prefix + "s", "Type": "Series", "Name": "Show", "ProviderIds": ids(map[string]string{"Tvdb": "1"})},
			{"Id": prefix + "s1", "Type": "Season", "SeriesId": prefix + "s", "ParentId": prefix + "s", "IndexNumber": 1},
		}
		for _, e := range eps {
			out = append(out, episode(prefix+"e"+string(rune('0'+e)), prefix+"s", prefix+"s1", 1, e))
		}
		return out
	}
	shared := movie("", "Shared", 2000, map[string]string{"Tmdb": "1"})
	a := append([]dto{
		withID(shared, "am-shared"),
		movie("am-only", "A Only", 2001, map[string]string{"Tmdb": "2"}),
	}, show("a", 1, 2)...)
	b := append([]dto{
		withID(shared, "bm-shared"),
		{"Id": "bmusic", "Type": "MusicAlbum", "Name": "Album", "AlbumArtist": "X"},
		{"Id": "btrack", "Type": "Audio", "Name": "T", "AlbumId": "bmusic", "ParentId": "bmusic", "IndexNumber": 1},
	}, show("b", 1)...)
	f.set("a", a...)
	f.set("b", b...)
}

func withID(d dto, id string) dto {
	out := dto{}
	for k, v := range d {
		out[k] = v
	}
	out["Id"] = id
	return out
}

func visible(t *testing.T, r *Resolver, upstream, id string) bool {
	t.Helper()
	return lookup(t, r, uid(t, r, upstream, id)).Visible
}

func expectVisible(t *testing.T, r *Resolver, want map[[2]string]bool) {
	t.Helper()
	for k, w := range want {
		if got := visible(t, r, k[0], k[1]); got != w {
			t.Errorf("%s/%s visible = %v, want %v", k[0], k[1], got, w)
		}
	}
}

func TestUnion(t *testing.T) {
	f := newFixture(t)
	library(f)
	r := f.resolver(Mode{Op: Union})
	expectVisible(t, r, map[[2]string]bool{
		{"a", "am-shared"}: true, {"a", "am-only"}: true, {"b", "btrack"}: true, {"a", "ae2"}: true, {"a", "as"}: true,
	})
	u := lookup(t, r, uid(t, r, "a", "am-shared"))
	if len(u.Refs) != 2 || !u.Refs[0].Source || !u.Refs[1].Source {
		t.Errorf("union must keep every copy as a source: %+v", u.Refs)
	}
}

func TestIntersection(t *testing.T) {
	f := newFixture(t)
	library(f)
	r := f.resolver(Mode{Op: Intersection})
	expectVisible(t, r, map[[2]string]bool{
		{"a", "am-shared"}: true,
		{"a", "am-only"}:   false,
		{"b", "btrack"}:    false, // music only on b
		{"b", "bmusic"}:    false, // container with no visible child
		{"a", "ae1"}:       true,  // episode on both
		{"a", "ae2"}:       false,
		{"a", "as"}:        true, // series visible through ae1
	})
}

func TestDifference(t *testing.T) {
	f := newFixture(t)
	library(f)
	r := f.resolver(Mode{Op: Difference, Base: "a", Subtract: []string{"b"}})
	expectVisible(t, r, map[[2]string]bool{
		{"a", "am-shared"}: false,
		{"a", "am-only"}:   true,
		{"b", "btrack"}:    false,
		{"a", "ae1"}:       false, // on both
		{"a", "ae2"}:       true,  // only on a
		{"a", "as"}:        true,  // shared series stays visible for its a-only episode
		{"a", "as1"}:       true,
	})
	series := lookup(t, r, uid(t, r, "a", "as"))
	for _, rf := range series.Refs {
		if rf.Source != (rf.Upstream == "a") {
			t.Errorf("difference sources must be base copies only: %+v", series.Refs)
		}
	}
}

func TestDifferenceIgnoresOtherUpstreams(t *testing.T) {
	f := newFixture(t)
	library(f)
	f.set("c", movie("cm", "C Only", 2003, map[string]string{"Tmdb": "3"}), movie("cm-a", "A Only", 2001, map[string]string{"Tmdb": "2"}))
	r := f.resolver(Mode{Op: Difference, Upstreams: []string{"a", "b", "c"}, Base: "a", Subtract: []string{"b"}})
	expectVisible(t, r, map[[2]string]bool{
		{"c", "cm"}:      false, // not on base
		{"a", "am-only"}: true,  // c is neither base nor subtracted
	})
	u := lookup(t, r, uid(t, r, "a", "am-only"))
	for _, rf := range u.Refs {
		if rf.Source != (rf.Upstream == "a") {
			t.Errorf("only base copies serve under difference: %+v", u.Refs)
		}
	}
}

func TestLazyAndLookup(t *testing.T) {
	f := newFixture(t)
	f.set("a", movie("0123456789abcdef0123456789abcdef", "M", 2000, nil))
	r := f.resolver(Mode{})

	indexed := uid(t, r, "a", "01234567-89AB-CDEF-0123-456789ABCDEF")
	if u := lookup(t, r, indexed); u.Lazy || u.Type != "Movie" {
		t.Errorf("indexed item resolved as %+v", u)
	}

	lazy := uid(t, r, "a", "fedcba98765432100123456789abcdef")
	if lazy != uid(t, r, "a", "fedcba98765432100123456789abcdef") {
		t.Error("lazy IDs must be stable")
	}
	u := lookup(t, r, lazy)
	if !u.Lazy || len(u.Refs) != 1 || u.Refs[0].Upstream != "a" || u.Refs[0].ID != "fedcba98765432100123456789abcdef" {
		t.Errorf("lazy lookup = %+v", u)
	}
	if _, err := r.Lookup(context.Background(), "ffffffffffffffffffffffffffffffff"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown id err = %v", err)
	}
}

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestNormalizeID(t *testing.T) {
	for in, want := range map[string]string{
		"0123456789ABCDEF0123456789abcdef":       "0123456789abcdef0123456789abcdef",
		"01234567-89ab-cdef-0123-456789abcdef":   "0123456789abcdef0123456789abcdef",
		"{01234567-89AB-CDEF-0123-456789ABCDEF}": "0123456789abcdef0123456789abcdef",
		"a-s1-1-12":                              "a-s1-1-12", // not a GUID: untouched
		"a-s11-1-2":                              "a-s11-1-2",
		"Artist-ID":                              "Artist-ID",
		"0123456789abcdef0123456789abcdeg":       "0123456789abcdef0123456789abcdeg",
	} {
		if got := NormalizeID(in); got != want {
			t.Errorf("NormalizeID(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPruneSessionAliases(t *testing.T) {
	ctx := context.Background()
	idx, err := index.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	r, err := New(idx, Mode{Op: Union, Upstreams: []string{"a"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	session, version := DeriveID("session"), DeriveID("version")
	if err := r.SetAliases(ctx, []Alias{
		{ID: session, Refs: []Ref{{Upstream: "a", ID: "00000000000000000000000000000001"}}, Session: true},
		{ID: version, Refs: []Ref{{Upstream: "a", ID: "00000000000000000000000000000002"}}},
	}); err != nil {
		t.Fatal(err)
	}
	minted, err := r.UnifiedID(ctx, "a", "00000000000000000000000000000003")
	if err != nil {
		t.Fatal(err)
	}
	if u, err := r.Lookup(ctx, session); err != nil || u.Minted {
		t.Fatalf("session alias: %+v %v", u, err)
	}
	if u, _ := r.Lookup(ctx, minted); !u.Minted {
		t.Error("minted ID not marked")
	}
	if n, err := r.PruneSessions(ctx, time.Now().Add(-time.Hour)); err != nil || n != 0 {
		t.Fatalf("pruned %d fresh rows (%v)", n, err)
	}
	if n, err := r.PruneSessions(ctx, time.Now().Add(time.Hour)); err != nil || n != 1 {
		t.Fatalf("pruned %d rows (%v), want the session alias only", n, err)
	}
	if _, err := r.Lookup(ctx, session); !errors.Is(err, ErrNotFound) {
		t.Errorf("session alias still resolves: %v", err)
	}
	for _, id := range []string{version, minted} {
		if _, err := r.Lookup(ctx, id); err != nil {
			t.Errorf("%s pruned: %v", id, err)
		}
	}
}

func TestKeysOfAndResolve(t *testing.T) {
	ctx := context.Background()
	movie := func(id, tmdb string) json.RawMessage {
		return json.RawMessage(`{"Id":"` + id + `","Type":"Movie","Name":"Film","ProductionYear":2001,"ProviderIds":{"Tmdb":"` + tmdb + `"}}`)
	}
	open := func(up, id string) *Resolver {
		idx, err := index.Open(filepath.Join(t.TempDir(), "db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { idx.Close() })
		fs, _ := idx.BeginFullSync(ctx, up)
		fs.PutViews(ctx, []json.RawMessage{json.RawMessage(`{"Id":"` + hexID("f1") + `","Name":"Movies","CollectionType":"movies"}`)})
		fs.PutItems(ctx, hexID("f1"), []json.RawMessage{movie(id, "603")})
		fs.Commit(ctx)
		r, err := New(idx, Mode{Op: Union, Upstreams: []string{up}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := r.Rebuild(ctx); err != nil {
			t.Fatal(err)
		}
		return r
	}
	a, b := open("home", hexID("a1")), open("laptop", hexID("b1"))
	ua, _ := a.UnifiedID(ctx, "home", hexID("a1"))
	keys, err := a.KeysOf(ctx, []string{ua})
	if err != nil || !slices.Contains(keys[ua], "movie:tmdb:603") {
		t.Fatalf("keys = %v %v", keys, err)
	}
	ub, _ := b.UnifiedID(ctx, "laptop", hexID("b1"))
	// Same match keys: the same ID on both instances anyway.
	if got := b.Resolve(ctx, ua, keys[ua]); got != ub {
		t.Errorf("Resolve = %s, want b's %s", got, ub)
	}
	// An ID b does not know, found by key.
	if got := b.Resolve(ctx, hexID("99"), keys[ua]); got != ub {
		t.Errorf("Resolve by key = %s, want %s", got, ub)
	}
	// Unknown everywhere: kept as is.
	if got := b.Resolve(ctx, hexID("98"), []string{"movie:tmdb:1"}); got != hexID("98") {
		t.Errorf("unknown = %s", got)
	}
}

func hexID(prefix string) string { return prefix + strings.Repeat("0", 32-len(prefix)) }
