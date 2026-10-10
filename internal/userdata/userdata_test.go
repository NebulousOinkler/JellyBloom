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

func TestApplyResumeRules(t *testing.T) {
	now := time.Now()
	movie := int64(7200) * 10_000_000 // 2h
	short := int64(120) * 10_000_000  // 2min
	cases := []struct {
		name       string
		runtime    int64
		stopAt     *int64
		wantPos    int64
		wantPlayed bool
	}{
		{"middle of a movie: resumable", movie, ticks(3600), 3600 * 10_000_000, false},
		{"under 5%: not resumable", movie, ticks(60), 0, false},
		{"over 90%: played", movie, ticks(7000), 0, true},
		{"short item: played on stop", short, ticks(60), 0, true},
		{"unknown runtime: played on stop", 0, ticks(60), 0, true},
		{"no position reported: unchanged", movie, nil, 42, false},
	}
	for _, c := range cases {
		r := Record{Position: 42}
		Apply(&r, Report{Event: Stop, PositionTicks: c.stopAt}, c.runtime, now)
		if r.Position != c.wantPos || r.Played != c.wantPlayed || !r.LastPlayed.Equal(now) {
			t.Errorf("%s: pos=%d played=%v", c.name, r.Position, r.Played)
		}
	}

	r := Record{}
	a, sub := 2, 3
	Apply(&r, Report{Event: Start, AudioStream: &a}, movie, now)
	Apply(&r, Report{Event: Progress, PositionTicks: ticks(100), SubtitleStream: &sub}, movie, now)
	if r.PlayCount != 1 || r.Position != 100*10_000_000 || *r.AudioStream != 2 || *r.SubtitleStream != 3 || !r.Resumable() {
		t.Errorf("start+progress = %+v", r)
	}
}

func TestStore(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	recs, err := s.Update(ctx, "u1", []string{"a", "b"}, func(r *Record) { MarkPlayed(r, time.UnixMilli(1000)) })
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
