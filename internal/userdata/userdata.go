// Package userdata keeps each JellyBloom user's state about items, keyed by
// unified item ID: played, play count, favorite, likes, resume position, and
// last played. It applies Jellyfin's playback-report rules, and stores
// playlists (per user) and collections (shared).
package userdata

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

const schema = `
CREATE TABLE IF NOT EXISTS user_data (
	user_id       TEXT NOT NULL,
	item_id       TEXT NOT NULL,
	played        INTEGER NOT NULL DEFAULT 0,
	play_count    INTEGER NOT NULL DEFAULT 0,
	is_favorite   INTEGER NOT NULL DEFAULT 0,
	likes         INTEGER,             -- NULL: no rating; 1 liked; 0 disliked
	position      INTEGER NOT NULL DEFAULT 0, -- resume position, ticks
	last_played   INTEGER NOT NULL DEFAULT 0, -- unix ms; 0 = never
	audio_stream  INTEGER,
	sub_stream    INTEGER,
	PRIMARY KEY (user_id, item_id)
);
CREATE INDEX IF NOT EXISTS user_data_resume ON user_data (user_id, last_played);

CREATE TABLE IF NOT EXISTS playlists (
	id         TEXT PRIMARY KEY,
	owner      TEXT NOT NULL,
	name       TEXT NOT NULL,
	media_type TEXT NOT NULL,
	created    INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS playlist_entries (
	entry_id    TEXT PRIMARY KEY,
	playlist_id TEXT NOT NULL,
	item_id     TEXT NOT NULL,
	position    INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS playlist_entries_list ON playlist_entries (playlist_id, position);

CREATE TABLE IF NOT EXISTS collections (
	id      TEXT PRIMARY KEY,
	name    TEXT NOT NULL,
	created INTEGER NOT NULL
);
-- Items of JellyBloom's collections, and edits to upstream collections
-- (keyed by the upstream collection's unified ID): hidden = 1 removes an
-- upstream member.
CREATE TABLE IF NOT EXISTS collection_items (
	collection_id TEXT NOT NULL,
	item_id       TEXT NOT NULL,
	added         INTEGER NOT NULL,
	hidden        INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (collection_id, item_id)
);
-- Deleted playlists, so a sync with another JellyBloom deletes them there
-- too instead of bringing them back.
CREATE TABLE IF NOT EXISTS playlist_deletions (
	id      TEXT PRIMARY KEY,
	owner   TEXT NOT NULL,
	deleted INTEGER NOT NULL -- unix ms
);
`

// migrations bring tables created by earlier versions up to date.
var migrations = []struct{ table, column, ddl string }{
	{"collection_items", "hidden", `ALTER TABLE collection_items ADD COLUMN hidden INTEGER NOT NULL DEFAULT 0`},
	// When a record or playlist last changed (unix ms), for sync.
	{"user_data", "updated", `ALTER TABLE user_data ADD COLUMN updated INTEGER NOT NULL DEFAULT 0`},
	{"playlists", "updated", `ALTER TABLE playlists ADD COLUMN updated INTEGER NOT NULL DEFAULT 0`},
}

// Record is one user's state for one item.
type Record struct {
	ItemID         string
	Played         bool
	PlayCount      int
	IsFavorite     bool
	Likes          *bool
	Position       int64 // ticks
	LastPlayed     time.Time
	AudioStream    *int
	SubtitleStream *int
}

// Resumable reports whether playback can resume part-way. As in Jellyfin,
// a played item being watched again is resumable too.
func (r Record) Resumable() bool { return r.Position > 0 }

// Store persists user data, playlists, and collections. Safe for concurrent use.
type Store struct {
	db  *sql.DB
	now func() time.Time
}

// Open creates the tables in db.
func Open(db *sql.DB) (*Store, error) {
	if _, err := db.Exec(schema); err != nil {
		return nil, err
	}
	for _, m := range migrations {
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM pragma_table_info(?) WHERE name = ?`, m.table, m.column).Scan(&n); err != nil {
			return nil, err
		}
		if n == 0 {
			if _, err := db.Exec(m.ddl); err != nil {
				return nil, err
			}
		}
	}
	return &Store{db: db, now: time.Now}, nil
}

const recordCols = `item_id, played, play_count, is_favorite, likes, position, last_played, audio_stream, sub_stream`

func scanRecord(sc interface{ Scan(...any) error }) (Record, error) {
	var r Record
	var likes, audio, sub sql.NullInt64
	var last int64
	err := sc.Scan(&r.ItemID, &r.Played, &r.PlayCount, &r.IsFavorite, &likes, &r.Position, &last, &audio, &sub)
	if likes.Valid {
		b := likes.Int64 == 1
		r.Likes = &b
	}
	if audio.Valid {
		v := int(audio.Int64)
		r.AudioStream = &v
	}
	if sub.Valid {
		v := int(sub.Int64)
		r.SubtitleStream = &v
	}
	if last > 0 {
		r.LastPlayed = time.UnixMilli(last)
	}
	return r, err
}

// Get returns the records that exist for ids.
func (s *Store) Get(ctx context.Context, userID string, ids []string) (map[string]Record, error) {
	out := map[string]Record{}
	for start := 0; start < len(ids); start += 400 {
		chunk := ids[start:min(start+400, len(ids))]
		args := []any{userID}
		for _, id := range chunk {
			args = append(args, id)
		}
		rows, err := s.db.QueryContext(ctx, `SELECT `+recordCols+` FROM user_data WHERE user_id = ? AND item_id IN (?`+
			strings.Repeat(",?", len(chunk)-1)+`)`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			r, err := scanRecord(rows)
			if err != nil {
				rows.Close()
				return nil, err
			}
			out[r.ItemID] = r
		}
		rows.Close()
	}
	return out, nil
}

// All returns every record of a user.
func (s *Store) All(ctx context.Context, userID string) (map[string]Record, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+recordCols+` FROM user_data WHERE user_id = ?`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]Record{}
	for rows.Next() {
		r, err := scanRecord(rows)
		if err != nil {
			return nil, err
		}
		out[r.ItemID] = r
	}
	return out, rows.Err()
}

// Update applies fn to the records of ids (missing ones start empty) in one
// transaction and returns the results.
func (s *Store) Update(ctx context.Context, userID string, ids []string, fn func(*Record)) (map[string]Record, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	// Writing first takes the write lock now (waiting out other writers), so
	// the reads below see the latest state and concurrent edits neither fail
	// nor overwrite each other. An empty row reads like a missing one.
	if len(ids) > 0 {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO user_data (user_id, item_id) VALUES (?, ?)`, userID, ids[0]); err != nil {
			return nil, err
		}
	}
	out := map[string]Record{}
	for _, id := range ids {
		r, err := scanRecord(tx.QueryRowContext(ctx, `SELECT `+recordCols+` FROM user_data WHERE user_id = ? AND item_id = ?`, userID, id))
		if errors.Is(err, sql.ErrNoRows) {
			r, err = Record{ItemID: id}, nil
		}
		if err != nil {
			return nil, err
		}
		fn(&r)
		var likes, audio, sub any
		if r.Likes != nil {
			likes = map[bool]int{true: 1, false: 0}[*r.Likes]
		}
		if r.AudioStream != nil {
			audio = *r.AudioStream
		}
		if r.SubtitleStream != nil {
			sub = *r.SubtitleStream
		}
		var last int64
		if !r.LastPlayed.IsZero() {
			last = r.LastPlayed.UnixMilli()
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO user_data (user_id, `+recordCols+`, updated) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (user_id, item_id) DO UPDATE SET played = excluded.played, play_count = excluded.play_count,
				is_favorite = excluded.is_favorite, likes = excluded.likes, position = excluded.position,
				last_played = excluded.last_played, audio_stream = excluded.audio_stream, sub_stream = excluded.sub_stream,
				updated = excluded.updated`,
			userID, id, r.Played, r.PlayCount, r.IsFavorite, likes, r.Position, last, audio, sub, s.now().UnixMilli())
		if err != nil {
			return nil, err
		}
		out[id] = r
	}
	return out, tx.Commit()
}

// FollowRedirects moves user data, playlist entries, and collection items
// from unified IDs that were merged into another (identity_redirects) to the
// surviving ID, combining state when both exist.
func (s *Store) FollowRedirects(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []string{
		// Combine into an existing record for the new ID...
		`UPDATE user_data AS n SET
			played = MAX(n.played, o.played), play_count = n.play_count + o.play_count,
			is_favorite = MAX(n.is_favorite, o.is_favorite), likes = COALESCE(n.likes, o.likes),
			position = CASE WHEN o.last_played > n.last_played THEN o.position ELSE n.position END,
			last_played = MAX(n.last_played, o.last_played), updated = MAX(n.updated, o.updated)
		FROM identity_redirects r JOIN user_data o ON o.item_id = r.old_id
		WHERE n.item_id = r.new_id AND n.user_id = o.user_id`,
		`DELETE FROM user_data WHERE item_id IN (SELECT old_id FROM identity_redirects)
			AND EXISTS (SELECT 1 FROM user_data n JOIN identity_redirects r ON n.item_id = r.new_id
				WHERE r.old_id = user_data.item_id AND n.user_id = user_data.user_id)`,
		// ...or just re-key it.
		`UPDATE user_data SET item_id = (SELECT new_id FROM identity_redirects WHERE old_id = item_id)
			WHERE item_id IN (SELECT old_id FROM identity_redirects)`,
		`UPDATE playlist_entries SET item_id = (SELECT new_id FROM identity_redirects WHERE old_id = item_id)
			WHERE item_id IN (SELECT old_id FROM identity_redirects)`,
		`UPDATE OR IGNORE collection_items SET item_id = (SELECT new_id FROM identity_redirects WHERE old_id = item_id)
			WHERE item_id IN (SELECT old_id FROM identity_redirects)`,
		`DELETE FROM collection_items WHERE item_id IN (SELECT old_id FROM identity_redirects)`,
		// Edits to an upstream collection follow it too.
		`UPDATE OR IGNORE collection_items SET collection_id = (SELECT new_id FROM identity_redirects WHERE old_id = collection_id)
			WHERE collection_id IN (SELECT old_id FROM identity_redirects)`,
		`DELETE FROM collection_items WHERE collection_id IN (SELECT old_id FROM identity_redirects)`,
	} {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	return tx.Commit()
}
