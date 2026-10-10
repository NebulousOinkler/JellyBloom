package index

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/NebulousOinkler/JellyBloom/internal/upstream"
)

// Source is the upstream API the syncer needs; *upstream.Client implements it.
type Source interface {
	Name() string
	Views(ctx context.Context) (*upstream.QueryResult, error)
	Items(ctx context.Context, q upstream.ItemsQuery) (*upstream.QueryResult, error)
	EachItemPage(ctx context.Context, q upstream.ItemsQuery, pageSize int, fn func(*upstream.QueryResult) error) error
	EachArtistPage(ctx context.Context, q upstream.ItemsQuery, pageSize int, fn func(*upstream.QueryResult) error) error
	Listen(ctx context.Context, handle func(upstream.Event)) error
}

// SyncFields are the optional DTO fields requested so the index holds full items.
// All exist in Jellyfin 10.9+; an unknown field makes Jellyfin reject the query.
var SyncFields = []string{
	"AirTime", "Chapters", "ChildCount", "CumulativeRunTimeTicks", "DateCreated",
	"DateLastMediaAdded", "Etag", "ExternalUrls", "Genres", "Height", "MediaSources",
	"MediaStreams", "OriginalTitle", "Overview", "ParentId", "Path", "People",
	"PrimaryImageAspectRatio", "ProductionLocations", "ProviderIds", "RecursiveItemCount",
	"RemoteTrailers", "SeriesPrimaryImage", "SeriesStudio", "SortName",
	"SpecialFeatureCount", "Studios", "Taglines", "Tags", "Width",
}

// skippedCollections are upstream libraries JellyBloom does not index.
var skippedCollections = []string{"livetv"}

// SyncerOptions tunes a Syncer. Zero values select defaults.
type SyncerOptions struct {
	PageSize       int           // items per page in full syncs; default 500
	BatchSize      int           // IDs per lookup in incremental syncs; default 100
	ResyncInterval time.Duration // periodic full sync; default 6h
	Debounce       time.Duration // window for coalescing LibraryChanged bursts; default 2s
	RetryMin       time.Duration // first retry after a failed full sync; default 30s
	OnChange       func()        // called after each committed full or incremental sync
	Logger         *slog.Logger
}

// Syncer keeps one upstream's index current: a full sync at startup, after
// every socket reconnect, and periodically, plus incremental updates from
// LibraryChanged events in between. All writes for the upstream happen on the
// Run goroutine, so full and incremental syncs never interleave.
type Syncer struct {
	store *Store
	src   Source
	opts  SyncerOptions
	log   *slog.Logger

	fullReq   chan struct{}
	changes   chan *upstream.LibraryChange
	connected atomic.Bool
	roots     atomic.Pointer[map[string]bool] // upstream library and library root folder IDs
}

// isLibraryRoot reports whether id is a library, or a library's physical root
// folder (Jellyfin reports scan progress for these).
func (s *Syncer) isLibraryRoot(id string) bool {
	m := s.roots.Load()
	return m != nil && (*m)[strings.ToLower(strings.ReplaceAll(id, "-", ""))]
}

// NewSyncer creates a syncer for src writing into store.
func NewSyncer(store *Store, src Source, opts SyncerOptions) *Syncer {
	if opts.PageSize <= 0 {
		opts.PageSize = 500
	}
	if opts.BatchSize <= 0 {
		opts.BatchSize = 100
	}
	if opts.ResyncInterval <= 0 {
		opts.ResyncInterval = 6 * time.Hour
	}
	if opts.Debounce <= 0 {
		opts.Debounce = 2 * time.Second
	}
	if opts.RetryMin <= 0 {
		opts.RetryMin = 30 * time.Second
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.OnChange == nil {
		opts.OnChange = func() {}
	}
	return &Syncer{
		store:   store,
		src:     src,
		opts:    opts,
		log:     opts.Logger.With("upstream", src.Name()),
		fullReq: make(chan struct{}, 1),
		changes: make(chan *upstream.LibraryChange, 256),
	}
}

// RequestFullSync schedules a full sync; requests coalesce.
func (s *Syncer) RequestFullSync() {
	select {
	case s.fullReq <- struct{}{}:
	default:
	}
}

func (s *Syncer) onEvent(e upstream.Event) {
	switch e.Type {
	case upstream.EventConnected:
		// The startup sync covers the first connection; after a reconnect,
		// changes made while disconnected were missed, so resync.
		if s.connected.Swap(true) {
			s.RequestFullSync()
		}
	case upstream.EventDisconnected:
		s.log.Warn("upstream socket disconnected", "err", e.Err)
	case upstream.EventMessage:
		s.log.Debug("upstream message", "type", e.MessageType, "data", string(e.Data))
		// A library finished scanning. Scans can change artwork and metadata
		// without any LibraryChanged message, so resync.
		if e.MessageType == "RefreshProgress" {
			var p struct{ ItemId, Progress string }
			if json.Unmarshal(e.Data, &p) == nil && p.Progress == "100" && s.isLibraryRoot(p.ItemId) {
				s.log.Debug("library scan finished; resyncing", "library", p.ItemId)
				s.RequestFullSync()
			}
		}
	case upstream.EventLibraryChanged:
		select {
		case s.changes <- e.Library:
		default:
			s.log.Warn("change queue full; falling back to full sync")
			s.RequestFullSync()
		}
	}
}

// Run syncs until ctx is cancelled and returns ctx.Err().
func (s *Syncer) Run(ctx context.Context) error {
	go s.src.Listen(ctx, s.onEvent)
	s.RequestFullSync()

	resync := time.NewTicker(s.opts.ResyncInterval)
	defer resync.Stop()
	var debounce, retry <-chan time.Time
	retryDelay := s.opts.RetryMin
	pending := map[string]bool{} // id -> true upsert, false remove (latest wins)
	needFull := false

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-s.fullReq:
			needFull = true
		case <-retry:
			retry = nil
			needFull = true
		case <-resync.C:
			needFull = true
		case lc := <-s.changes:
			mergeChange(pending, lc)
			if debounce == nil {
				debounce = time.After(s.opts.Debounce)
			}
		case <-debounce:
			debounce = nil
			if err := s.applyChanges(ctx, pending); err != nil && ctx.Err() == nil {
				s.log.Error("incremental sync failed; scheduling full sync", "err", err)
				_ = s.store.SetError(ctx, s.src.Name(), err)
				needFull = true
			}
			pending = map[string]bool{}
		}

		if needFull {
			needFull = false
			if err := s.FullSync(ctx); err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				s.log.Error("full sync failed", "err", err, "retry_in", retryDelay)
				retry = time.After(retryDelay)
				retryDelay = min(retryDelay*2, s.opts.ResyncInterval)
			} else {
				retry = nil
				retryDelay = s.opts.RetryMin
			}
		}
	}
}

func mergeChange(pending map[string]bool, lc *upstream.LibraryChange) {
	for _, ids := range [][]string{lc.ItemsAdded, lc.ItemsUpdated} {
		for _, id := range ids {
			pending[id] = true
		}
	}
	for _, id := range lc.ItemsRemoved {
		pending[id] = false
	}
}

// FullSync replaces the upstream's snapshot with a fresh one. On failure the
// previous snapshot keeps serving.
func (s *Syncer) FullSync(ctx context.Context) (err error) {
	start := time.Now()
	fs, err := s.store.BeginFullSync(ctx, s.src.Name())
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = fs.Abort(context.WithoutCancel(ctx), err)
		}
	}()

	views, err := s.src.Views(ctx)
	if err != nil {
		return fmt.Errorf("views: %w", err)
	}
	var kept []json.RawMessage
	var headers []dtoHeader
	for _, raw := range views.Items {
		h, err := parseHeader(raw)
		if err != nil {
			return err
		}
		if slices.Contains(skippedCollections, h.CollectionType) {
			continue
		}
		kept = append(kept, raw)
		headers = append(headers, h)
	}
	if err := fs.PutViews(ctx, kept); err != nil {
		return err
	}
	norm := func(id string) string { return strings.ToLower(strings.ReplaceAll(id, "-", "")) }
	roots := map[string]bool{}
	for _, h := range headers {
		roots[norm(h.Id)] = true
	}
	// Folders whose parent is not indexed are library root folders.
	seen := map[string]bool{}
	folders := map[string]string{} // folder -> parent


	items := 0
	for _, v := range headers {
		put := func(p *upstream.QueryResult) error {
			items += len(p.Items)
			for _, raw := range p.Items {
				if h, err := parseHeader(raw); err == nil {
					seen[norm(h.Id)] = true
					if h.Type == "Folder" {
						folders[norm(h.Id)] = norm(h.ParentId)
					}
				}
			}
			return fs.PutItems(ctx, v.Id, p.Items)
		}
		q := upstream.ItemsQuery{
			ParentID:  v.Id,
			Recursive: true,
			Fields:    SyncFields,
			// Paging by creation date keeps items added mid-sync at the end,
			// so they don't shift earlier pages and cause skips.
			SortBy: []string{"DateCreated", "SortName"},
		}
		if err := s.src.EachItemPage(ctx, q, s.opts.PageSize, put); err != nil {
			return fmt.Errorf("items of view %s: %w", v.Name, err)
		}
		if v.CollectionType == "music" {
			aq := upstream.ItemsQuery{ParentID: v.Id, Fields: SyncFields, SortBy: []string{"SortName"}}
			if err := s.src.EachArtistPage(ctx, aq, s.opts.PageSize, put); err != nil {
				return fmt.Errorf("artists of view %s: %w", v.Name, err)
			}
		}
	}
	// Record library roots before the commit makes this sync visible, so a
	// scan-finished message arriving right after it is recognized.
	for f, parent := range folders {
		if !seen[parent] {
			roots[f] = true
		}
	}
	s.roots.Store(&roots)
	if err := fs.Commit(ctx); err != nil {
		return err
	}
	s.opts.OnChange()
	s.log.Info("full sync complete", "views", len(headers), "items", items, "took", time.Since(start).Round(time.Millisecond))
	return nil
}

// applyChanges re-fetches changed items, resolving which views each belongs
// to, and deletes removed ones. Items no longer visible upstream are removed.
// Library-level changes trigger a full sync instead.
func (s *Syncer) applyChanges(ctx context.Context, pending map[string]bool) error {
	if len(pending) == 0 {
		return nil
	}
	views, err := s.store.Views(ctx, s.src.Name())
	if err != nil {
		return err
	}
	viewIDs := map[string]bool{}
	for _, v := range views {
		viewIDs[v.ID] = true
	}

	var upsertIDs, removed []string
	for id, upsert := range pending {
		if viewIDs[id] {
			s.RequestFullSync() // a library itself changed
			continue
		}
		if upsert {
			upsertIDs = append(upsertIDs, id)
		} else {
			removed = append(removed, id)
		}
	}
	slices.Sort(upsertIDs)

	upserts := map[string][]json.RawMessage{}
	for batch := range slices.Chunk(upsertIDs, s.opts.BatchSize) {
		found := map[string]bool{}
		for _, v := range views {
			res, err := s.src.Items(ctx, upstream.ItemsQuery{IDs: batch, ParentID: v.ID, Recursive: true, Fields: SyncFields})
			if err != nil {
				return err
			}
			for _, raw := range res.Items {
				h, err := parseHeader(raw)
				if err != nil {
					return err
				}
				found[h.Id] = true
				upserts[v.ID] = append(upserts[v.ID], raw)
			}
		}
		var orphans []string
		for _, id := range batch {
			if !found[id] {
				orphans = append(orphans, id)
			}
		}
		if len(orphans) == 0 {
			continue
		}
		// Not under any known library: either a new library (resync) or an
		// item outside the libraries (index it without a view), or gone.
		res, err := s.src.Items(ctx, upstream.ItemsQuery{IDs: orphans, Fields: SyncFields})
		if err != nil {
			return err
		}
		seen := map[string]bool{}
		for _, raw := range res.Items {
			h, err := parseHeader(raw)
			if err != nil {
				return err
			}
			seen[h.Id] = true
			if h.Type == "CollectionFolder" || h.Type == "UserView" {
				s.RequestFullSync()
				continue
			}
			upserts[""] = append(upserts[""], raw)
		}
		for _, id := range orphans {
			if !seen[id] {
				removed = append(removed, id)
			}
		}
	}

	if err := s.store.ApplyChanges(ctx, s.src.Name(), upserts, removed); err != nil {
		return err
	}
	s.opts.OnChange()
	s.log.Debug("incremental sync applied", "upserted", len(upsertIDs), "removed", len(removed))
	return nil
}
