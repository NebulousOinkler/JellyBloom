package userdata

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// clock gives a store a controllable time.
type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func (c *clock) tick() { c.t = c.t.Add(time.Second) }

// sync merges a and b into each other, as `jellybloom sync` does, through
// JSON like the HTTP endpoints.
func syncStores(t *testing.T, a, b *Store, resolveA, resolveB ResolveFunc) {
	t.Helper()
	ctx := context.Background()
	roundTrip := func(s *Snapshot) *Snapshot {
		raw, err := json.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		var out Snapshot
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatal(err)
		}
		return &out
	}
	keys := func(ctx context.Context, ids []string) (map[string][]string, error) {
		out := map[string][]string{}
		for _, id := range ids {
			out[id] = []string{"key:" + id}
		}
		return out, nil
	}
	sa, err := a.Export(ctx, keys)
	if err != nil {
		t.Fatal(err)
	}
	sb, err := b.Export(ctx, keys)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Merge(ctx, roundTrip(sa), resolveB); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Merge(ctx, roundTrip(sb), resolveA); err != nil {
		t.Fatal(err)
	}
}

func record(t *testing.T, s *Store, user, item string) Record {
	t.Helper()
	m, err := s.Get(context.Background(), user, []string{item})
	if err != nil {
		t.Fatal(err)
	}
	return m[item]
}

func TestSyncWatchState(t *testing.T) {
	ctx := context.Background()
	a, b := open(t), open(t)
	ca, cb := &clock{time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)}, &clock{time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)}
	a.now, b.now = ca.now, cb.now

	// Different items on each side.
	a.Update(ctx, "u", []string{"x"}, func(r *Record) { r.IsFavorite = true })
	b.Update(ctx, "u", []string{"y"}, func(r *Record) { MarkPlayed(r, nil, cb.t) })
	// The same item on both: b's change is later and wins on both sides.
	a.Update(ctx, "u", []string{"z"}, func(r *Record) { r.Position = 100 })
	cb.tick()
	b.Update(ctx, "u", []string{"z"}, func(r *Record) { r.Position = 200 })
	// Same time, different state: both sides settle on the same one.
	a.Update(ctx, "u", []string{"w"}, func(r *Record) { r.Position = 1 })
	ca.t = cb.t
	a.Update(ctx, "u", []string{"w"}, func(r *Record) { r.Position = 1 })
	b.Update(ctx, "u", []string{"w"}, func(r *Record) { r.Position = 2 })

	syncStores(t, a, b, nil, nil)
	for _, s := range []*Store{a, b} {
		if !record(t, s, "u", "x").IsFavorite || !record(t, s, "u", "y").Played {
			t.Error("one-sided changes did not reach both")
		}
		if got := record(t, s, "u", "z").Position; got != 200 {
			t.Errorf("z position = %d, want the later change (200)", got)
		}
	}
	if pa, pb := record(t, a, "u", "w").Position, record(t, b, "u", "w").Position; pa != pb {
		t.Errorf("tie left the sides apart: %d vs %d", pa, pb)
	}
	// Syncing again changes nothing.
	before, _ := a.Export(ctx, nil)
	syncStores(t, a, b, nil, nil)
	after, _ := a.Export(ctx, nil)
	if len(before.UserData) != len(after.UserData) {
		t.Errorf("second sync changed records: %d -> %d", len(before.UserData), len(after.UserData))
	}
	// Unfavoriting later propagates (it is a change, not an absence).
	ca.t = cb.t.Add(time.Minute)
	a.Update(ctx, "u", []string{"x"}, func(r *Record) { r.IsFavorite = false })
	syncStores(t, a, b, nil, nil)
	if record(t, b, "u", "x").IsFavorite {
		t.Error("unfavorite did not propagate")
	}
}

func TestSyncMapsItemsByKeys(t *testing.T) {
	ctx := context.Background()
	a, b := open(t), open(t)
	a.Update(ctx, "u", []string{"a-movie"}, func(r *Record) { r.Played = true })
	// b knows the same movie (key "key:a-movie") as "b-movie".
	resolveB := func(_ context.Context, id string, keys []string) string {
		for _, k := range keys {
			if k == "key:a-movie" {
				return "b-movie"
			}
		}
		return id
	}
	syncStores(t, a, b, nil, resolveB)
	if !record(t, b, "u", "b-movie").Played {
		t.Error("record not found under b's ID")
	}
	if _, ok := mustGet(t, b, "u", "a-movie"); ok {
		t.Error("record also stored under a's ID")
	}
}

func mustGet(t *testing.T, s *Store, user, item string) (Record, bool) {
	m, err := s.Get(context.Background(), user, []string{item})
	if err != nil {
		t.Fatal(err)
	}
	r, ok := m[item]
	return r, ok
}

func TestSyncPlaylists(t *testing.T) {
	ctx := context.Background()
	a, b := open(t), open(t)
	c := &clock{time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)}
	a.now, b.now = c.now, c.now

	p, err := a.CreatePlaylist(ctx, "u", "Road trip", "Video", []string{"m1", "m2"})
	if err != nil {
		t.Fatal(err)
	}
	syncStores(t, a, b, nil, nil)
	got, err := b.Playlist(ctx, "u", p.ID)
	if err != nil || got.Name != "Road trip" || len(got.Entries) != 2 || got.Entries[0].ID != p.Entries[0].ID {
		t.Fatalf("playlist on b = %+v, %v", got, err)
	}
	// Edited on b later: b's version wins on a.
	c.tick()
	if err := b.AddToPlaylist(ctx, "u", p.ID, []string{"m3"}); err != nil {
		t.Fatal(err)
	}
	syncStores(t, a, b, nil, nil)
	if got, _ := a.Playlist(ctx, "u", p.ID); len(got.Entries) != 3 {
		t.Errorf("a after b's edit: %d entries", len(got.Entries))
	}
	// Deleted on a: gone from b, and not brought back.
	c.tick()
	if err := a.DeletePlaylist(ctx, "u", p.ID); err != nil {
		t.Fatal(err)
	}
	syncStores(t, a, b, nil, nil)
	for name, s := range map[string]*Store{"a": a, "b": b} {
		if _, err := s.Playlist(ctx, "u", p.ID); err != ErrNotFound {
			t.Errorf("deleted playlist on %s: %v", name, err)
		}
	}
	// A playlist edited after the other side deleted it survives.
	q, _ := a.CreatePlaylist(ctx, "u", "Keep", "Video", []string{"m1"})
	syncStores(t, a, b, nil, nil)
	c.tick()
	b.DeletePlaylist(ctx, "u", q.ID)
	c.tick()
	a.AddToPlaylist(ctx, "u", q.ID, []string{"m2"})
	syncStores(t, a, b, nil, nil)
	if got, err := b.Playlist(ctx, "u", q.ID); err != nil || len(got.Entries) != 2 {
		t.Errorf("playlist edited after deletion: %+v %v", got, err)
	}
}
