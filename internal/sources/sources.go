// Package sources tracks every upstream copy of a file (a "source") and
// decides which one serves each play. It fingerprints copies to tell
// identical files from different or broken ones, measures each upstream's
// speed, tracks health with quarantine and backoff, and relays static
// streams with byte-level failover between identical copies.
//
// It persists digests, verdicts, and health only: never file contents.
// Sampled blocks are read through a hash and dropped.
package sources

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"sync"
	"time"
)

const schema = `
-- Sampled-content digest of each copy (see Offsets): 8 bytes per sample.
CREATE TABLE IF NOT EXISTS source_fingerprints (
	upstream TEXT NOT NULL,
	media    TEXT NOT NULL,
	item     TEXT NOT NULL,
	size     INTEGER NOT NULL,
	digest   BLOB NOT NULL,
	checked  INTEGER NOT NULL, -- unix ms
	PRIMARY KEY (upstream, media)
);
CREATE INDEX IF NOT EXISTS source_fingerprints_content ON source_fingerprints (size, digest);

-- Content verdicts: a copy found truncated or damaged against a peer, or
-- disputed (differs from its peer with no way to tell which is intact).
CREATE TABLE IF NOT EXISTS source_verdicts (
	upstream      TEXT NOT NULL,
	media         TEXT NOT NULL,
	size          INTEGER NOT NULL,
	state         TEXT NOT NULL,
	reason        TEXT NOT NULL,
	peer_upstream TEXT NOT NULL,
	peer_media    TEXT NOT NULL,
	offset        INTEGER NOT NULL, -- evidence: where the copies differ
	length        INTEGER NOT NULL,
	checked       INTEGER NOT NULL,
	PRIMARY KEY (upstream, media)
);

-- Quarantine after repeated failures, with its backoff.
CREATE TABLE IF NOT EXISTS source_health (
	upstream TEXT NOT NULL,
	media    TEXT NOT NULL,
	item     TEXT NOT NULL,
	reason   TEXT NOT NULL,
	until    INTEGER NOT NULL, -- unix ms
	backoff  INTEGER NOT NULL, -- ms
	PRIMARY KEY (upstream, media)
);

-- Hashes of 1 MiB blocks seen while relaying copies that have identical
-- peers, so a block that differs between them is caught during playback.
CREATE TABLE IF NOT EXISTS source_blocks (
	upstream TEXT NOT NULL,
	media    TEXT NOT NULL,
	block    INTEGER NOT NULL,
	size     INTEGER NOT NULL,
	hash     INTEGER NOT NULL,
	PRIMARY KEY (upstream, media, block)
) WITHOUT ROWID;
`

// Source is one upstream copy of a file: a media source of an item.
type Source struct {
	Upstream string
	Item     string // real item ID
	Media    string // real media source ID
}

func (s Source) String() string { return s.Upstream + "/" + s.Media }

// key identifies a source independently of the item that holds it.
type key struct{ up, media string }

func (s Source) key() key { return key{s.Upstream, s.Media} }

// Copy is a source with the metadata its upstream reported.
type Copy struct {
	Source
	Size int64  // bytes; 0 when unknown (content checks are skipped)
	Key  string // encode identity without the size (EncodeKey)
}

// Opener sends one request to an upstream. The registry uses JellyBloom's
// own upstream sessions for checks and pings.
type Opener func(ctx context.Context, upstream, method, path string, query url.Values, hdr http.Header) (*http.Response, error)

// Library lists playable items and their copies, for background checks and
// the redundancy report.
type Library interface {
	EachItem(ctx context.Context, fn func(LibraryItem) error) error
}

// LibraryFunc adapts a function to Library.
type LibraryFunc func(ctx context.Context, fn func(LibraryItem) error) error

func (f LibraryFunc) EachItem(ctx context.Context, fn func(LibraryItem) error) error {
	return f(ctx, fn)
}

// LibraryItem is a visible unified item and its source copies' DTOs.
type LibraryItem struct {
	ID     string
	Type   string
	Copies []LibraryCopy
}

// LibraryCopy is one upstream's DTO of an item.
type LibraryCopy struct {
	Upstream string
	Item     string // real item ID
	DTO      json.RawMessage
}

// Options configures a Registry.
type Options struct {
	Upstreams []string // config order: the tie-breaker when speeds are alike
	Open      Opener   // nil: no network (e.g. the report CLI)
	Library   Library  // nil: no background checks or report items
	// StallTimeout is how long a relay waits for upstream bytes before
	// failing over (default 10s).
	StallTimeout time.Duration
	Logger       *slog.Logger
	Now          func() time.Time
}

// Registry is safe for concurrent use.
type Registry struct {
	db    *sql.DB
	opts  Options
	log   *slog.Logger
	now   func() time.Time
	order map[string]int

	mu       sync.Mutex
	stats    map[string]*upstreamStats
	health   map[key]*health
	verdicts map[key]Verdict
	sessions map[string]*Session
	pairs    map[[2]key]bool // truncation checks that found no prefix
	checking map[key]bool    // block disagreements being resolved
	kick     chan struct{}   // wakes the background verifier
}

// New opens the registry's tables in db and loads persisted state.
func New(db *sql.DB, o Options) (*Registry, error) {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.StallTimeout <= 0 {
		o.StallTimeout = 10 * time.Second
	}
	if _, err := db.Exec(schema); err != nil {
		return nil, fmt.Errorf("sources: init schema: %w", err)
	}
	r := &Registry{
		db: db, opts: o, log: o.Logger, now: o.Now, order: map[string]int{},
		stats: map[string]*upstreamStats{}, health: map[key]*health{}, verdicts: map[key]Verdict{},
		sessions: map[string]*Session{}, pairs: map[[2]key]bool{}, checking: map[key]bool{},
		kick: make(chan struct{}, 1),
	}
	for i, name := range o.Upstreams {
		r.order[name] = i
		r.stats[name] = &upstreamStats{}
	}
	if err := r.load(context.Background()); err != nil {
		return nil, err
	}
	return r, nil
}

// load reads verdicts and quarantines into memory.
func (r *Registry) load(ctx context.Context) error {
	rows, err := r.db.QueryContext(ctx, `SELECT upstream, media, size, state, reason, peer_upstream, peer_media, offset, length, checked FROM source_verdicts`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var v Verdict
		var k key
		if err := rows.Scan(&k.up, &k.media, &v.Size, &v.State, &v.Reason, &v.Peer.Upstream, &v.Peer.Media, &v.Offset, &v.Length, &v.Checked); err != nil {
			return err
		}
		r.verdicts[k] = v
	}
	if err := rows.Err(); err != nil {
		return err
	}
	rows, err = r.db.QueryContext(ctx, `SELECT upstream, media, item, reason, until, backoff FROM source_health`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var k key
		h := &health{}
		var until, backoff int64
		if err := rows.Scan(&k.up, &k.media, &h.item, &h.reason, &until, &backoff); err != nil {
			return err
		}
		h.until, h.backoff = time.UnixMilli(until), time.Duration(backoff)*time.Millisecond
		h.failures = quarantineAfter
		r.health[k] = h
	}
	return rows.Err()
}

// Kick asks the background verifier to look for unchecked copies soon (e.g.
// after the library changed).
func (r *Registry) Kick() {
	select {
	case r.kick <- struct{}{}:
	default:
	}
}
