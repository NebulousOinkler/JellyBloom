package userdata

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"slices"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func open(t *testing.T) *Store {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	// Created by the identity package in production.
	if _, err := db.Exec(`CREATE TABLE identity_redirects (old_id TEXT PRIMARY KEY, new_id TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	s, err := Open(db)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func ticks(sec int64) *int64 { v := sec * 10_000_000; return &v }

// The expectations below were observed on a real Jellyfin 12.2 server with
// testrig (same steps, compared field by field).
func TestApplyMatchesJellyfin(t *testing.T) {
	t0 := time.UnixMilli(1_000_000)
	movie := Item{Runtime: 600 * 10_000_000, Kind: KindVideo}
	short := Item{Runtime: 20 * 10_000_000, Kind: KindVideo}
	track := Item{Runtime: 360 * 10_000_000, Kind: KindAudio}
	book := Item{Runtime: 720 * 10_000_000, Kind: KindAudioBook}
	start := func(pos *int64) Report { return Report{Event: Start, PositionTicks: pos} }
	progress := func(pos *int64) Report { return Report{Event: Progress, PositionTicks: pos} }
	stop := func(pos *int64) Report { return Report{Event: Stop, PositionTicks: pos} }
	type want struct {
		played bool
		count  int
		pos    int64 // seconds
		dated  bool  // LastPlayed moved to this step's time
	}
	cases := []struct {
		name  string
		item  Item
		steps []Report
		want  []want
	}{
		{"movie: watch, rewatch, finish", movie,
			[]Report{start(ticks(300)), progress(ticks(18)), progress(ticks(300)), stop(ticks(300)), start(nil), progress(ticks(570)), stop(ticks(570)), start(ticks(0)), stop(ticks(12))},
			[]want{{false, 1, 0, true}, {false, 1, 0, false}, {false, 1, 300, true}, {false, 1, 300, true}, {false, 2, 300, true},
				{true, 2, 0, true}, {true, 2, 0, true}, {true, 3, 0, false}, {true, 3, 0, false}}},
		{"movie: stop without position", movie, []Report{stop(nil)}, []want{{true, 1, 0, true}}},
		{"short video: a resume point counts as played", short, []Report{start(ticks(0)), progress(ticks(10))}, []want{{false, 1, 0, true}, {true, 1, 0, true}}},
		{"music: played on start, never resumable", track,
			[]Report{start(ticks(0)), progress(ticks(180)), stop(ticks(180))}, []want{{true, 1, 0, true}, {true, 1, 0, false}, {true, 1, 0, false}}},
		{"audiobook: minute rules", book,
			[]Report{start(ticks(0)), progress(ticks(120)), progress(ticks(360)), stop(ticks(360)), start(nil), stop(ticks(600))},
			[]want{{false, 1, 0, true}, {false, 1, 0, false}, {false, 1, 360, true}, {false, 1, 360, true}, {false, 2, 360, true}, {true, 2, 0, true}}},
		{"audiobook: stop early, then without position", book, []Report{stop(ticks(29)), stop(nil)}, []want{{false, 0, 0, false}, {true, 1, 0, true}}},
		{"unknown runtime: played", Item{}, []Report{stop(ticks(5))}, []want{{true, 0, 0, true}}},
	}
	for _, c := range cases {
		var r Record
		for i, rep := range c.steps {
			now := t0.Add(time.Duration(i+1) * time.Minute)
			Apply(&r, rep, c.item, now)
			w := c.want[i]
			if r.Played != w.played || r.PlayCount != w.count || r.Position != w.pos*10_000_000 || r.LastPlayed.Equal(now) != w.dated {
				t.Errorf("%s, step %d: played=%v count=%d pos=%ds dated=%v; want %+v",
					c.name, i+1, r.Played, r.PlayCount, r.Position/10_000_000, r.LastPlayed.Equal(now), w)
			}
		}
	}

	r := Record{}
	a, sub := 2, 3
	Apply(&r, Report{Event: Start, AudioStream: &a}, movie, t0)
	Apply(&r, Report{Event: Progress, SubtitleStream: &sub}, movie, t0)
	if *r.AudioStream != 2 || *r.SubtitleStream != 3 {
		t.Errorf("stream choices = %+v", r)
	}
	r = Record{Played: true, Position: 5}
	if !r.Resumable() {
		t.Error("a played item with a resume point (a rewatch) is resumable, as in Jellyfin")
	}
}

func TestMarkPlayedMatchesJellyfin(t *testing.T) {
	now, then := time.UnixMilli(5000), time.UnixMilli(1000)
	var r Record
	MarkPlayed(&r, nil, now)
	if !r.Played || r.PlayCount != 1 || !r.LastPlayed.Equal(now) {
		t.Errorf("first mark = %+v", r)
	}
	MarkPlayed(&r, nil, now.Add(time.Hour))
	if r.PlayCount != 1 || !r.LastPlayed.Equal(now) {
		t.Errorf("marking again without a date keeps count and date: %+v", r)
	}
	MarkPlayed(&r, &then, now)
	if r.PlayCount != 2 || !r.LastPlayed.Equal(then) {
		t.Errorf("marking with a date counts a play on that date: %+v", r)
	}
	r.Position = 7
	MarkUnplayed(&r)
	if r != (Record{}) {
		t.Errorf("unplayed clears everything Jellyfin clears: %+v", r)
	}
}

func TestStore(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	at := time.UnixMilli(1000)
	recs, err := s.Update(ctx, "u1", []string{"a", "b"}, func(r *Record) { MarkPlayed(r, &at, at) })
	if err != nil || !recs["a"].Played || recs["b"].PlayCount != 1 {
		t.Fatalf("Update = %+v, %v", recs, err)
	}
	s.Update(ctx, "u1", []string{"a"}, func(r *Record) {
		yes := true
		r.IsFavorite, r.Likes = true, &yes
	})
	got, _ := s.Get(ctx, "u1", []string{"a", "missing"})
	if len(got) != 1 || !got["a"].IsFavorite || !*got["a"].Likes || !got["a"].Played || got["a"].LastPlayed.UnixMilli() != 1000 {
		t.Errorf("Get = %+v", got)
	}
	if other, _ := s.All(ctx, "u2"); len(other) != 0 {
		t.Error("user data leaked across users")
	}
	d := got["a"].DTO("a", 0)
	if d["Played"] != true || d["IsFavorite"] != true || d["Likes"] != true || d["LastPlayedDate"] == nil {
		t.Errorf("DTO = %v", d)
	}
}

func TestFollowRedirects(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	// old1 only has data: re-keyed. old2 and new2 both: combined.
	s.Update(ctx, "u", []string{"old1"}, func(r *Record) { r.IsFavorite = true })
	s.Update(ctx, "u", []string{"old2"}, func(r *Record) { r.Played, r.PlayCount, r.LastPlayed = true, 2, time.UnixMilli(5000) })
	s.Update(ctx, "u", []string{"new2"}, func(r *Record) { r.PlayCount, r.Position, r.LastPlayed = 1, 77, time.UnixMilli(1000) })
	p, _ := s.CreatePlaylist(ctx, "u", "P", "Video", []string{"old1", "x"})
	c, _ := s.CreateCollection(ctx, "C", []string{"old2", "new2"})
	s.db.Exec(`INSERT INTO identity_redirects VALUES ('old1', 'new1'), ('old2', 'new2')`)

	if err := s.FollowRedirects(ctx); err != nil {
		t.Fatal(err)
	}
	all, _ := s.All(ctx, "u")
	if len(all) != 2 || !all["new1"].IsFavorite {
		t.Errorf("records = %+v", all)
	}
	if n := all["new2"]; !n.Played || n.PlayCount != 3 || n.Position != 0 || n.LastPlayed.UnixMilli() != 5000 {
		t.Errorf("combined = %+v (newer position wins)", n)
	}
	if p, _ = s.Playlist(ctx, "u", p.ID); !slices.Equal(p.ItemIDs(), []string{"new1", "x"}) {
		t.Errorf("playlist = %v", p.ItemIDs())
	}
	if c, _ = s.Collection(ctx, c.ID); !slices.Equal(c.Items, []string{"new2"}) {
		t.Errorf("collection = %v", c.Items)
	}
}

func TestPlaylists(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	p, err := s.CreatePlaylist(ctx, "u", "Road trip", "Audio", []string{"a", "b"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Playlist(ctx, "someone-else", p.ID); !errors.Is(err, ErrNotFound) {
		t.Error("playlists must be private to their owner")
	}
	s.AddToPlaylist(ctx, "u", p.ID, []string{"c", "a"}) // duplicates allowed
	p, _ = s.Playlist(ctx, "u", p.ID)
	if !slices.Equal(p.ItemIDs(), []string{"a", "b", "c", "a"}) {
		t.Fatalf("items = %v", p.ItemIDs())
	}
	last := p.Entries[3].ID
	s.MovePlaylistEntry(ctx, "u", p.ID, last, 0)
	s.RemoveFromPlaylist(ctx, "u", p.ID, []string{p.Entries[1].ID})
	p, _ = s.Playlist(ctx, "u", p.ID)
	if !slices.Equal(p.ItemIDs(), []string{"a", "a", "c"}) || p.Entries[0].ID != last {
		t.Errorf("after move+remove = %v", p.ItemIDs())
	}
	s.UpdatePlaylist(ctx, "u", p.ID, "Renamed", []string{"z"})
	p, _ = s.Playlist(ctx, "u", p.ID)
	if p.Name != "Renamed" || !slices.Equal(p.ItemIDs(), []string{"z"}) {
		t.Errorf("updated = %s %v", p.Name, p.ItemIDs())
	}
	if err := s.DeletePlaylist(ctx, "someone-else", p.ID); !errors.Is(err, ErrNotFound) {
		t.Error("only the owner may delete")
	}
	s.DeletePlaylist(ctx, "u", p.ID)
	if ps, _ := s.Playlists(ctx, "u"); len(ps) != 0 {
		t.Error("not deleted")
	}
}

func TestCollections(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	c, _ := s.CreateCollection(ctx, "Trilogy", []string{"a", "b"})
	s.AddToCollection(ctx, c.ID, []string{"b", "c"})
	s.RemoveFromCollection(ctx, c.ID, []string{"a"})
	c, _ = s.Collection(ctx, c.ID)
	if !slices.Equal(c.Items, []string{"b", "c"}) {
		t.Errorf("items = %v", c.Items)
	}
	if all, _ := s.Collections(ctx); len(all) != 1 {
		t.Error("collections are shared and listed")
	}
	s.DeleteCollection(ctx, c.ID)
	if _, err := s.Collection(ctx, c.ID); !errors.Is(err, ErrNotFound) {
		t.Error("not deleted")
	}
}
