package index

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NebulousOinkler/JellyBloom/internal/upstream"
)

// fakeSource is an in-memory upstream.
type fakeSource struct {
	mu      sync.Mutex
	views   []map[string]any
	items   map[string]map[string]any // id -> dto
	members map[string][]string       // view id -> item ids (recursive)
	artists map[string][]string       // view id -> artist ids
	failAll error
	queries int
	handler func(upstream.Event)
	ready   chan struct{}
}

func newFakeSource() *fakeSource {
	return &fakeSource{items: map[string]map[string]any{}, members: map[string][]string{}, artists: map[string][]string{}, ready: make(chan struct{})}
}

func (f *fakeSource) Name() string { return "up" }

func (f *fakeSource) addView(id, name, ctype string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.views = append(f.views, map[string]any{"Id": id, "Name": name, "CollectionType": ctype, "Type": "CollectionFolder"})
	f.items[id] = map[string]any{"Id": id, "Name": name, "Type": "CollectionFolder"}
}

func (f *fakeSource) addItem(view, id, typ, name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.items[id] = map[string]any{"Id": id, "Name": name, "Type": typ, "ParentId": view}
	f.members[view] = append(f.members[view], id)
}

func (f *fakeSource) addArtist(view, id, name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.items[id] = map[string]any{"Id": id, "Name": name, "Type": "MusicArtist"}
	f.artists[view] = append(f.artists[view], id)
}

func (f *fakeSource) rename(id, name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.items[id]["Name"] = name
}

func (f *fakeSource) move(id, from, to string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.members[from] = slices.DeleteFunc(f.members[from], func(s string) bool { return s == id })
	f.members[to] = append(f.members[to], id)
}

func (f *fakeSource) remove(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.items, id)
	for v := range f.members {
		f.members[v] = slices.DeleteFunc(f.members[v], func(s string) bool { return s == id })
	}
}

func (f *fakeSource) setFail(err error) {
	f.mu.Lock()
	f.failAll = err
	f.mu.Unlock()
}

func raws(ms []map[string]any) []json.RawMessage {
	var out []json.RawMessage
	for _, m := range ms {
		b, _ := json.Marshal(m)
		out = append(out, b)
	}
	return out
}

func (f *fakeSource) Views(ctx context.Context) (*upstream.QueryResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failAll != nil {
		return nil, f.failAll
	}
	return &upstream.QueryResult{Items: raws(f.views), TotalRecordCount: len(f.views)}, nil
}

func (f *fakeSource) Items(ctx context.Context, q upstream.ItemsQuery) (*upstream.QueryResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queries++
	if f.failAll != nil {
		return nil, f.failAll
	}
	if !slices.Contains(q.Fields, "ProviderIds") {
		return nil, errors.New("sync must request full fields")
	}
	var ids []string
	if q.ParentID != "" {
		ids = slices.Clone(f.members[q.ParentID])
	} else {
		for id := range f.items {
			ids = append(ids, id)
		}
		slices.Sort(ids)
	}
	if q.IDs != nil {
		ids = slices.DeleteFunc(ids, func(id string) bool { return !slices.Contains(q.IDs, id) })
	}
	return f.page(ids, q), nil
}

func (f *fakeSource) page(ids []string, q upstream.ItemsQuery) *upstream.QueryResult {
	total := len(ids)
	start := min(q.StartIndex, total)
	end := total
	if q.Limit > 0 {
		end = min(start+q.Limit, total)
	}
	var page []map[string]any
	for _, id := range ids[start:end] {
		page = append(page, f.items[id])
	}
	return &upstream.QueryResult{Items: raws(page), TotalRecordCount: total, StartIndex: start}
}

func eachPage(ctx context.Context, get func(upstream.ItemsQuery) (*upstream.QueryResult, error), q upstream.ItemsQuery, size int, fn func(*upstream.QueryResult) error) error {
	q.Limit = size
	for {
		p, err := get(q)
		if err != nil {
			return err
		}
		if len(p.Items) == 0 {
			return nil
		}
		if err := fn(p); err != nil {
			return err
		}
		q.StartIndex += len(p.Items)
		if q.StartIndex >= p.TotalRecordCount {
			return nil
		}
	}
}

func (f *fakeSource) EachItemPage(ctx context.Context, q upstream.ItemsQuery, size int, fn func(*upstream.QueryResult) error) error {
	return eachPage(ctx, func(q upstream.ItemsQuery) (*upstream.QueryResult, error) { return f.Items(ctx, q) }, q, size, fn)
}

func (f *fakeSource) EachArtistPage(ctx context.Context, q upstream.ItemsQuery, size int, fn func(*upstream.QueryResult) error) error {
	return eachPage(ctx, func(q upstream.ItemsQuery) (*upstream.QueryResult, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.failAll != nil {
			return nil, f.failAll
		}
		return f.page(f.artists[q.ParentID], q), nil
	}, q, size, fn)
}

func (f *fakeSource) Listen(ctx context.Context, handle func(upstream.Event)) error {
	f.mu.Lock()
	f.handler = handle
	f.mu.Unlock()
	close(f.ready)
	handle(upstream.Event{Upstream: "up", Type: upstream.EventConnected})
	<-ctx.Done()
	return ctx.Err()
}

func (f *fakeSource) emit(e upstream.Event) {
	f.mu.Lock()
	h := f.handler
	f.mu.Unlock()
	h(e)
}

func openStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func library() *fakeSource {
	f := newFakeSource()
	f.addView("vMovies", "Movies", "movies")
	f.addView("vMusic", "Music", "music")
	f.addView("vTV", "Live TV", "livetv")
	for i := range 7 {
		f.addItem("vMovies", fmt.Sprintf("m%d", i), "Movie", fmt.Sprintf("Movie %d", i))
	}
	f.addItem("vMusic", "album1", "MusicAlbum", "Album")
	f.addItem("vMusic", "track1", "Audio", "Track")
	f.addArtist("vMusic", "artist1", "Artist")
	f.addItem("vTV", "ch1", "TvChannel", "Channel")
	return f
}

func ids(t *testing.T, s *Store) []string {
	t.Helper()
	got, err := s.ItemIDs(context.Background(), "up")
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestFullSync(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	f := library()
	sy := NewSyncer(s, f, SyncerOptions{PageSize: 3})

	if err := sy.FullSync(ctx); err != nil {
		t.Fatal(err)
	}
	want := []string{"album1", "artist1", "m0", "m1", "m2", "m3", "m4", "m5", "m6", "track1"}
	if got := ids(t, s); !slices.Equal(got, want) {
		t.Errorf("items = %v, want %v (livetv skipped, artists included)", got, want)
	}
	views, _ := s.Views(ctx, "up")
	if len(views) != 2 || views[0].Name != "Movies" || views[1].CollectionType != "music" {
		t.Errorf("views = %+v", views)
	}
	it, _ := s.Item(ctx, "up", "artist1")
	if it == nil || it.Type != "MusicArtist" || !slices.Equal(it.Views, []string{"vMusic"}) {
		t.Errorf("artist1 = %+v", it)
	}
	var dto map[string]any
	_ = json.Unmarshal(it.DTO, &dto)
	if dto["Name"] != "Artist" {
		t.Errorf("DTO not stored verbatim: %s", it.DTO)
	}
	st, _ := s.Status(ctx, "up")
	if st.Generation != 1 || st.Items != 10 || st.Views != 2 || st.LastFullSync.IsZero() || st.LastError != "" {
		t.Errorf("status = %+v", st)
	}
}

func TestFullSyncReplacesSnapshot(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	f := library()
	sy := NewSyncer(s, f, SyncerOptions{PageSize: 3})
	if err := sy.FullSync(ctx); err != nil {
		t.Fatal(err)
	}

	f.remove("m0")
	f.rename("m1", "Renamed")
	f.addItem("vMovies", "m9", "Movie", "New")

	// Mid-sync, readers still see the old snapshot.
	fs, err := s.BeginFullSync(ctx, "up")
	if err != nil {
		t.Fatal(err)
	}
	staged := raws([]map[string]any{{"Id": "m9", "Name": "New", "Type": "Movie"}, {"Id": "ghost", "Name": "Deleted before resync", "Type": "Movie"}})
	if err := fs.PutItems(ctx, "vMovies", staged); err != nil {
		t.Fatal(err)
	}
	if got := ids(t, s); !slices.Contains(got, "m0") {
		t.Error("m0 disappeared before commit")
	}
	if err := fs.Abort(ctx, errors.New("interrupted")); err != nil {
		t.Fatal(err)
	}
	if st, _ := s.Status(ctx, "up"); st.LastError != "interrupted" || st.Generation != 1 {
		t.Errorf("after abort: %+v", st)
	}

	if err := sy.FullSync(ctx); err != nil {
		t.Fatal(err)
	}
	got := ids(t, s)
	if slices.Contains(got, "m0") || slices.Contains(got, "ghost") || !slices.Contains(got, "m9") {
		t.Errorf("items after resync = %v", got)
	}
	if it, _ := s.Item(ctx, "up", "m1"); it.Name != "Renamed" {
		t.Errorf("m1 name = %q", it.Name)
	}
	if st, _ := s.Status(ctx, "up"); st.LastError != "" || st.Generation != 3 {
		t.Errorf("after resync: %+v", st)
	}
}

func TestFailedFullSyncKeepsSnapshot(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	f := library()
	sy := NewSyncer(s, f, SyncerOptions{})
	if err := sy.FullSync(ctx); err != nil {
		t.Fatal(err)
	}
	f.setFail(errors.New("upstream down"))
	if err := sy.FullSync(ctx); err == nil {
		t.Fatal("expected error")
	}
	if got := ids(t, s); len(got) != 10 {
		t.Errorf("snapshot lost: %v", got)
	}
	if st, _ := s.Status(ctx, "up"); !strings.Contains(st.LastError, "upstream down") {
		t.Errorf("LastError = %q", st.LastError)
	}
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func startSyncer(t *testing.T, s *Store, f *fakeSource, opts SyncerOptions) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	sy := NewSyncer(s, f, opts)
	go func() { sy.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	<-f.ready
	eventually(t, "initial sync", func() bool { st, _ := s.Status(ctx, "up"); return st.Generation == 1 })
}

func changed(f *fakeSource, lc upstream.LibraryChange) {
	f.emit(upstream.Event{Upstream: "up", Type: upstream.EventLibraryChanged, Library: &lc})
}

func TestIncrementalSync(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	f := library()
	f.addView("vKids", "Kids", "movies")
	startSyncer(t, s, f, SyncerOptions{Debounce: 10 * time.Millisecond})

	f.addItem("vMovies", "m10", "Movie", "Added")
	f.rename("m2", "Updated")
	f.move("m3", "vMovies", "vKids")
	f.remove("m4")
	f.remove("m5") // reported as updated, but gone: must be removed
	changed(f, upstream.LibraryChange{ItemsAdded: []string{"m10"}, ItemsUpdated: []string{"m2", "m3", "m5"}})
	changed(f, upstream.LibraryChange{ItemsRemoved: []string{"m4"}})

	eventually(t, "changes applied", func() bool {
		it, _ := s.Item(ctx, "up", "m10")
		gone, _ := s.Item(ctx, "up", "m5")
		return it != nil && gone == nil
	})
	if it, _ := s.Item(ctx, "up", "m2"); it.Name != "Updated" {
		t.Errorf("m2 = %q", it.Name)
	}
	if it, _ := s.Item(ctx, "up", "m3"); !slices.Equal(it.Views, []string{"vKids"}) {
		t.Errorf("m3 views = %v, want moved to vKids only", it.Views)
	}
	if it, _ := s.Item(ctx, "up", "m4"); it != nil {
		t.Error("m4 not removed")
	}
	if st, _ := s.Status(ctx, "up"); st.Generation != 1 || st.LastChange.IsZero() {
		t.Errorf("incremental sync should not bump generation: %+v", st)
	}
}

func TestReconnectAndNewLibraryTriggerFullSync(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	f := library()
	startSyncer(t, s, f, SyncerOptions{Debounce: 10 * time.Millisecond})

	// Reconnect: changes during the outage were missed, so a full sync runs.
	f.addItem("vMovies", "missed", "Movie", "Missed")
	f.emit(upstream.Event{Upstream: "up", Type: upstream.EventDisconnected})
	f.emit(upstream.Event{Upstream: "up", Type: upstream.EventConnected})
	eventually(t, "resync after reconnect", func() bool { it, _ := s.Item(ctx, "up", "missed"); return it != nil })

	// A new library shows up as an added CollectionFolder.
	f.addView("vDocs", "Docs", "movies")
	f.addItem("vDocs", "d1", "Movie", "Doc")
	changed(f, upstream.LibraryChange{ItemsAdded: []string{"vDocs"}})
	eventually(t, "resync after new library", func() bool {
		views, _ := s.Views(ctx, "up")
		it, _ := s.Item(ctx, "up", "d1")
		return len(views) == 3 && it != nil
	})
}

func TestFullSyncRetriesAfterFailure(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	f := library()
	f.setFail(errors.New("starting up"))
	sy := NewSyncer(s, f, SyncerOptions{RetryMin: 20 * time.Millisecond})
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { sy.Run(runCtx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })

	eventually(t, "error recorded", func() bool { st, _ := s.Status(ctx, "up"); return st.LastError != "" })
	f.setFail(nil)
	eventually(t, "retry succeeded", func() bool { st, _ := s.Status(ctx, "up"); return st.Items == 10 && st.LastError == "" })
}

func TestLibraryScanFinishTriggersFullSync(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	f := library()
	startSyncer(t, s, f, SyncerOptions{Debounce: 10 * time.Millisecond})

	// An artwork-only change: no LibraryChanged, but the scan's progress
	// reaches 100 for the library.
	f.rename("m6", "New artwork, same item")
	msg := func(item, progress string) {
		f.emit(upstream.Event{Upstream: "up", Type: upstream.EventMessage, MessageType: "RefreshProgress",
			Data: json.RawMessage(`{"ItemId":"` + item + `","Progress":"` + progress + `"}`)})
	}
	msg("vMovies", "40")
	msg("m1", "100") // not a library: ignored
	time.Sleep(50 * time.Millisecond)
	if st, _ := s.Status(ctx, "up"); st.Generation != 1 {
		t.Fatalf("resynced on a non-final or non-library progress message (generation %d)", st.Generation)
	}
	msg("vMovies", "100")
	// Staged rows are readable before a sync commits, so wait for the commit.
	eventually(t, "resync after scan", func() bool {
		st, _ := s.Status(ctx, "up")
		it, _ := s.Item(ctx, "up", "m6")
		return st.Generation >= 2 && it != nil && it.Name == "New artwork, same item"
	})

	// Jellyfin reports completion for a library's physical root folder, a
	// Folder whose parent is not indexed.
	f.addItem("vMovies", "rootfolder", "Folder", "movies")
	f.mu.Lock()
	f.items["rootfolder"]["ParentId"] = "aggregate-root"
	f.mu.Unlock()
	before, _ := s.Status(ctx, "up")
	f.emit(upstream.Event{Upstream: "up", Type: upstream.EventConnected}) // reconnect -> resync learns the root
	// Roots are recorded when the sync commits, so wait for the commit rather
	// than for the folder to appear.
	eventually(t, "resync committed", func() bool { st, _ := s.Status(ctx, "up"); return st.Generation > before.Generation })
	f.rename("m5", "Changed during scan")
	msg("rootfolder", "100")
	eventually(t, "resync after root folder scan", func() bool { it, _ := s.Item(ctx, "up", "m5"); return it != nil && it.Name == "Changed during scan" })
}
