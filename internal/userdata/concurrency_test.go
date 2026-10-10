package userdata

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	"github.com/NebulousOinkler/JellyBloom/internal/index"
)

// Concurrent edits (two devices reporting at once, a mark during playback)
// must neither fail nor lose updates, on the production database settings.
func TestConcurrentEdits(t *testing.T) {
	ctx := context.Background()
	idx, err := index.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { idx.Close() })
	if _, err := idx.DB().Exec(`CREATE TABLE IF NOT EXISTS identity_redirects (old_id TEXT PRIMARY KEY, new_id TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	s, err := Open(idx.DB())
	if err != nil {
		t.Fatal(err)
	}
	p, _ := s.CreatePlaylist(ctx, "u", "P", "Video", nil)
	const n = 40
	var wg sync.WaitGroup
	errs := make(chan error, 2*n)
	for range n {
		wg.Go(func() {
			if _, err := s.Update(ctx, "u", []string{"item"}, func(r *Record) { r.PlayCount++ }); err != nil {
				errs <- err
			}
		})
		wg.Go(func() {
			if err := s.AddToPlaylist(ctx, "u", p.ID, []string{"x"}); err != nil {
				errs <- err
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent edit failed: %v", err)
		break
	}
	recs, _ := s.Get(ctx, "u", []string{"item"})
	if recs["item"].PlayCount != n {
		t.Errorf("play count = %d, want %d (lost updates)", recs["item"].PlayCount, n)
	}
	if p, _ = s.Playlist(ctx, "u", p.ID); len(p.Entries) != n {
		t.Errorf("playlist entries = %d, want %d (lost updates)", len(p.Entries), n)
	}
}
