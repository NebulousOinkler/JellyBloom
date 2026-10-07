// Package index keeps JellyBloom's local copy of upstream item metadata.
// It stores DTO JSON only; media and other upstream files are never stored.
package index

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS sync_state (
	upstream        TEXT PRIMARY KEY,
	generation      INTEGER NOT NULL DEFAULT 0, -- committed snapshot
	staged          INTEGER NOT NULL DEFAULT 0, -- highest generation ever begun
	last_full_sync  INTEGER NOT NULL DEFAULT 0, -- unix ms of last successful full sync
	last_change     INTEGER NOT NULL DEFAULT 0, -- unix ms of last applied incremental change
	last_error      TEXT    NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS views (
	upstream        TEXT NOT NULL,
	real_id         TEXT NOT NULL,
	name            TEXT NOT NULL,
	collection_type TEXT NOT NULL,
	dto             BLOB NOT NULL,
	generation      INTEGER NOT NULL,
	PRIMARY KEY (upstream, real_id)
);

CREATE TABLE IF NOT EXISTS items (
	upstream   TEXT NOT NULL,
	real_id    TEXT NOT NULL,
	type       TEXT NOT NULL,
	name       TEXT NOT NULL,
	parent_id  TEXT NOT NULL,
	dto        BLOB NOT NULL,
	generation INTEGER NOT NULL,
	synced_at  INTEGER NOT NULL,
	PRIMARY KEY (upstream, real_id)
);
CREATE INDEX IF NOT EXISTS items_gen    ON items (upstream, generation);
CREATE INDEX IF NOT EXISTS items_parent ON items (upstream, parent_id);
CREATE INDEX IF NOT EXISTS items_type   ON items (type);

-- Library membership: which upstream views an item appears under.
CREATE TABLE IF NOT EXISTS item_views (
	upstream   TEXT NOT NULL,
	real_id    TEXT NOT NULL,
	view_id    TEXT NOT NULL,
	generation INTEGER NOT NULL,
	PRIMARY KEY (upstream, real_id, view_id)
);
CREATE INDEX IF NOT EXISTS item_views_gen  ON item_views (upstream, generation);
CREATE INDEX IF NOT EXISTS item_views_view ON item_views (upstream, view_id);
`

// Store is the SQLite-backed metadata index. It is safe for concurrent use.
type Store struct {
	db  *sql.DB
	now func() time.Time
}

// Open opens (creating if needed) the index database at path.
func Open(path string) (*Store, error) {
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("index: init schema: %w", err)
	}
	return &Store{db: db, now: time.Now}, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// Item is one indexed upstream item.
type Item struct {
	Upstream string
	ID       string
	Type     string
	Name     string
	ParentID string
	DTO      json.RawMessage
	Views    []string // upstream view IDs the item appears under
	SyncedAt time.Time
}

// View is one indexed upstream library.
type View struct {
	Upstream       string
	ID             string
	Name           string
	CollectionType string
	DTO            json.RawMessage
}

// Status describes an upstream's sync state.
type Status struct {
	Upstream     string
	Generation   int64
	LastFullSync time.Time
	LastChange   time.Time
	LastError    string
	Items        int
	Views        int
}

// dtoHeader is the subset of a BaseItemDto the index needs as columns.
type dtoHeader struct {
	Id             string
	Name           string
	Type           string
	ParentId       string
	CollectionType string
}

func parseHeader(raw json.RawMessage) (dtoHeader, error) {
	var h dtoHeader
	if err := json.Unmarshal(raw, &h); err != nil {
		return h, fmt.Errorf("index: bad item JSON: %w", err)
	}
	if h.Id == "" {
		return h, errors.New("index: item JSON has no Id")
	}
	return h, nil
}

// generation returns the upstream's committed generation, creating its state row.
func generation(ctx context.Context, tx *sql.Tx, upstream string) (int64, error) {
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO sync_state (upstream) VALUES (?)`, upstream); err != nil {
		return 0, err
	}
	var gen int64
	err := tx.QueryRowContext(ctx, `SELECT generation FROM sync_state WHERE upstream = ?`, upstream).Scan(&gen)
	return gen, err
}

func (s *Store) withTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

func putItems(ctx context.Context, tx *sql.Tx, upstream, viewID string, gen int64, now time.Time, items []json.RawMessage) error {
	itemStmt, err := tx.PrepareContext(ctx, `
		INSERT INTO items (upstream, real_id, type, name, parent_id, dto, generation, synced_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (upstream, real_id) DO UPDATE SET
			type = excluded.type, name = excluded.name, parent_id = excluded.parent_id,
			dto = excluded.dto, generation = excluded.generation, synced_at = excluded.synced_at`)
	if err != nil {
		return err
	}
	defer itemStmt.Close()
	viewStmt, err := tx.PrepareContext(ctx, `
		INSERT INTO item_views (upstream, real_id, view_id, generation) VALUES (?, ?, ?, ?)
		ON CONFLICT (upstream, real_id, view_id) DO UPDATE SET generation = excluded.generation`)
	if err != nil {
		return err
	}
	defer viewStmt.Close()

	for _, raw := range items {
		h, err := parseHeader(raw)
		if err != nil {
			return err
		}
		if _, err := itemStmt.ExecContext(ctx, upstream, h.Id, h.Type, h.Name, h.ParentId, []byte(raw), gen, now.UnixMilli()); err != nil {
			return err
		}
		if viewID != "" {
			if _, err := viewStmt.ExecContext(ctx, upstream, h.Id, viewID, gen); err != nil {
				return err
			}
		}
	}
	return nil
}

// FullSync stages a complete snapshot of one upstream under a new generation.
// Readers keep seeing the previous snapshot's items until Commit, which drops
// everything the new snapshot did not touch.
type FullSync struct {
	s        *Store
	upstream string
	gen      int64
}

// BeginFullSync starts a full sync for upstream. Generations only increase,
// even across aborted syncs, so rows staged by an aborted sync are always
// older than the next sync's and get dropped by its Commit.
func (s *Store) BeginFullSync(ctx context.Context, upstream string) (*FullSync, error) {
	var gen int64
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		if _, err := generation(ctx, tx, upstream); err != nil {
			return err
		}
		return tx.QueryRowContext(ctx, `
			UPDATE sync_state SET staged = MAX(staged, generation) + 1 WHERE upstream = ?
			RETURNING staged`, upstream).Scan(&gen)
	})
	if err != nil {
		return nil, err
	}
	return &FullSync{s: s, upstream: upstream, gen: gen}, nil
}

// PutViews records the upstream's libraries.
func (f *FullSync) PutViews(ctx context.Context, views []json.RawMessage) error {
	return f.s.withTx(ctx, func(tx *sql.Tx) error {
		for _, raw := range views {
			h, err := parseHeader(raw)
			if err != nil {
				return err
			}
			_, err = tx.ExecContext(ctx, `
				INSERT INTO views (upstream, real_id, name, collection_type, dto, generation) VALUES (?, ?, ?, ?, ?, ?)
				ON CONFLICT (upstream, real_id) DO UPDATE SET
					name = excluded.name, collection_type = excluded.collection_type,
					dto = excluded.dto, generation = excluded.generation`,
				f.upstream, h.Id, h.Name, h.CollectionType, []byte(raw), f.gen)
			if err != nil {
				return err
			}
		}
		return nil
	})
}

// PutItems records one page of items found under viewID.
func (f *FullSync) PutItems(ctx context.Context, viewID string, items []json.RawMessage) error {
	return f.s.withTx(ctx, func(tx *sql.Tx) error {
		return putItems(ctx, tx, f.upstream, viewID, f.gen, f.s.now(), items)
	})
}

// Commit atomically replaces the previous snapshot with this one.
func (f *FullSync) Commit(ctx context.Context) error {
	return f.s.withTx(ctx, func(tx *sql.Tx) error {
		for _, q := range []string{
			`DELETE FROM items      WHERE upstream = ? AND generation < ?`,
			`DELETE FROM item_views WHERE upstream = ? AND generation < ?`,
			`DELETE FROM views      WHERE upstream = ? AND generation < ?`,
		} {
			if _, err := tx.ExecContext(ctx, q, f.upstream, f.gen); err != nil {
				return err
			}
		}
		_, err := tx.ExecContext(ctx, `UPDATE sync_state SET generation = ?, last_full_sync = ?, last_error = '' WHERE upstream = ?`,
			f.gen, f.s.now().UnixMilli(), f.upstream)
		return err
	})
}

// Abort records a failed full sync. Staged rows stay (they are newer than
// what they replaced) and are reconciled by the next successful full sync.
func (f *FullSync) Abort(ctx context.Context, cause error) error {
	return f.s.SetError(ctx, f.upstream, cause)
}

// SetError records the latest sync error for upstream.
func (s *Store) SetError(ctx context.Context, upstream string, cause error) error {
	msg := ""
	if cause != nil {
		msg = cause.Error()
	}
	return s.withTx(ctx, func(tx *sql.Tx) error {
		if _, err := generation(ctx, tx, upstream); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE sync_state SET last_error = ? WHERE upstream = ?`, msg, upstream)
		return err
	})
}

// ApplyChanges upserts items (keyed by the view each was found under; "" for
// none) and deletes removed IDs, all in the upstream's current generation.
func (s *Store) ApplyChanges(ctx context.Context, upstream string, upserts map[string][]json.RawMessage, removed []string) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		gen, err := generation(ctx, tx, upstream)
		if err != nil {
			return err
		}
		now := s.now()
		for _, id := range removed {
			if _, err := tx.ExecContext(ctx, `DELETE FROM items WHERE upstream = ? AND real_id = ?`, upstream, id); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM item_views WHERE upstream = ? AND real_id = ?`, upstream, id); err != nil {
				return err
			}
		}
		// Upserted items get their library membership replaced, not appended to,
		// so an item moved between libraries leaves its old view.
		for _, items := range upserts {
			for _, raw := range items {
				h, err := parseHeader(raw)
				if err != nil {
					return err
				}
				if _, err := tx.ExecContext(ctx, `DELETE FROM item_views WHERE upstream = ? AND real_id = ?`, upstream, h.Id); err != nil {
					return err
				}
			}
		}
		for viewID, items := range upserts {
			if err := putItems(ctx, tx, upstream, viewID, gen, now, items); err != nil {
				return err
			}
		}
		_, err = tx.ExecContext(ctx, `UPDATE sync_state SET last_change = ? WHERE upstream = ?`, now.UnixMilli(), upstream)
		return err
	})
}

// Item returns one indexed item, or nil if it is not indexed.
func (s *Store) Item(ctx context.Context, upstream, id string) (*Item, error) {
	it := Item{Upstream: upstream, ID: id}
	var dto []byte
	var synced int64
	err := s.db.QueryRowContext(ctx, `SELECT type, name, parent_id, dto, synced_at FROM items WHERE upstream = ? AND real_id = ?`,
		upstream, id).Scan(&it.Type, &it.Name, &it.ParentID, &dto, &synced)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	it.DTO = dto
	it.SyncedAt = time.UnixMilli(synced)
	rows, err := s.db.QueryContext(ctx, `SELECT view_id FROM item_views WHERE upstream = ? AND real_id = ? ORDER BY view_id`, upstream, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		it.Views = append(it.Views, v)
	}
	return &it, rows.Err()
}

// Views returns the indexed libraries of upstream, ordered by name.
func (s *Store) Views(ctx context.Context, upstream string) ([]View, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT real_id, name, collection_type, dto FROM views WHERE upstream = ? ORDER BY name, real_id`, upstream)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []View
	for rows.Next() {
		v := View{Upstream: upstream}
		var dto []byte
		if err := rows.Scan(&v.ID, &v.Name, &v.CollectionType, &dto); err != nil {
			return nil, err
		}
		v.DTO = dto
		out = append(out, v)
	}
	return out, rows.Err()
}

// ItemIDs returns all indexed item IDs for upstream, sorted.
func (s *Store) ItemIDs(ctx context.Context, upstream string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT real_id FROM items WHERE upstream = ? ORDER BY real_id`, upstream)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// Status returns the sync state of upstream. An upstream never synced has a
// zero Generation.
func (s *Store) Status(ctx context.Context, upstream string) (Status, error) {
	st := Status{Upstream: upstream}
	var full, change int64
	err := s.db.QueryRowContext(ctx, `SELECT generation, last_full_sync, last_change, last_error FROM sync_state WHERE upstream = ?`,
		upstream).Scan(&st.Generation, &full, &change, &st.LastError)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return st, err
	}
	if full > 0 {
		st.LastFullSync = time.UnixMilli(full)
	}
	if change > 0 {
		st.LastChange = time.UnixMilli(change)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM items WHERE upstream = ?`, upstream).Scan(&st.Items); err != nil {
		return st, err
	}
	err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM views WHERE upstream = ?`, upstream).Scan(&st.Views)
	return st, err
}
