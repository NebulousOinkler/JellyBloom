package userdata

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Snapshot is an instance's user state in a form another JellyBloom can
// merge: per-user item state, playlists, and playlist deletions. Items carry
// their match keys, so an instance that knows an item under another ID still
// finds it.
type Snapshot struct {
	Version          int
	UserData         []SyncRecord
	Playlists        []SyncPlaylist
	DeletedPlaylists []SyncDeletion
}

// SnapshotVersion is the format Export writes and Merge accepts.
const SnapshotVersion = 1

// SyncRecord is one user's state for one item.
type SyncRecord struct {
	User           string
	Item           string
	Keys           []string `json:",omitempty"`
	Played         bool
	PlayCount      int
	IsFavorite     bool
	Likes          *bool `json:",omitempty"`
	Position       int64 // ticks
	LastPlayed     int64 // unix ms; 0 = never
	AudioStream    *int  `json:",omitempty"`
	SubtitleStream *int  `json:",omitempty"`
	Updated        int64 // unix ms of the last change
}

// SyncPlaylist is a playlist with its entries in order.
type SyncPlaylist struct {
	ID, Owner, Name, MediaType string
	Created, Updated           int64 // unix ms
	Entries                    []SyncEntry
}

// SyncEntry is one playlist entry.
type SyncEntry struct {
	ID   string
	Item string
	Keys []string `json:",omitempty"`
}

// SyncDeletion records a deleted playlist.
type SyncDeletion struct {
	ID, Owner string
	Deleted   int64 // unix ms
}

// KeysFunc returns the match keys of item IDs.
type KeysFunc func(ctx context.Context, ids []string) (map[string][]string, error)

// ResolveFunc maps an item ID from another instance, with its match keys,
// to this instance's ID for the same item.
type ResolveFunc func(ctx context.Context, id string, keys []string) string

// Export returns every user's state.
func (s *Store) Export(ctx context.Context, keysOf KeysFunc) (*Snapshot, error) {
	snap := &Snapshot{Version: SnapshotVersion}
	rows, err := s.db.QueryContext(ctx, `SELECT user_id, `+recordCols+`, updated FROM user_data ORDER BY user_id, item_id`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var user string
		var updated int64
		r, err := scanRecord(scanFunc(func(dest ...any) error {
			return rows.Scan(append(append([]any{&user}, dest...), &updated)...)
		}))
		if err != nil {
			rows.Close()
			return nil, err
		}
		snap.UserData = append(snap.UserData, toSync(user, r, updated))
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	rows, err = s.db.QueryContext(ctx, `SELECT id, owner, name, media_type, created, updated FROM playlists ORDER BY id`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var p SyncPlaylist
		if err := rows.Scan(&p.ID, &p.Owner, &p.Name, &p.MediaType, &p.Created, &p.Updated); err != nil {
			rows.Close()
			return nil, err
		}
		snap.Playlists = append(snap.Playlists, p)
	}
	rows.Close()
	for i := range snap.Playlists {
		es, err := playlistEntries(ctx, s.db, snap.Playlists[i].ID)
		if err != nil {
			return nil, err
		}
		for _, e := range es {
			snap.Playlists[i].Entries = append(snap.Playlists[i].Entries, SyncEntry{ID: e.ID, Item: e.ItemID})
		}
	}

	rows, err = s.db.QueryContext(ctx, `SELECT id, owner, deleted FROM playlist_deletions ORDER BY id`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var d SyncDeletion
		if err := rows.Scan(&d.ID, &d.Owner, &d.Deleted); err != nil {
			rows.Close()
			return nil, err
		}
		snap.DeletedPlaylists = append(snap.DeletedPlaylists, d)
	}
	rows.Close()

	if keysOf != nil {
		var ids []string
		for _, r := range snap.UserData {
			ids = append(ids, r.Item)
		}
		for _, p := range snap.Playlists {
			for _, e := range p.Entries {
				ids = append(ids, e.Item)
			}
		}
		slices.Sort(ids)
		keys, err := keysOf(ctx, slices.Compact(ids))
		if err != nil {
			return nil, err
		}
		for i := range snap.UserData {
			snap.UserData[i].Keys = keys[snap.UserData[i].Item]
		}
		for i := range snap.Playlists {
			for j := range snap.Playlists[i].Entries {
				snap.Playlists[i].Entries[j].Keys = keys[snap.Playlists[i].Entries[j].Item]
			}
		}
	}
	return snap, nil
}

type scanFunc func(dest ...any) error

func (f scanFunc) Scan(dest ...any) error { return f(dest...) }

func toSync(user string, r Record, updated int64) SyncRecord {
	sr := SyncRecord{User: user, Item: r.ItemID, Played: r.Played, PlayCount: r.PlayCount, IsFavorite: r.IsFavorite,
		Likes: r.Likes, Position: r.Position, AudioStream: r.AudioStream, SubtitleStream: r.SubtitleStream, Updated: updated}
	if !r.LastPlayed.IsZero() {
		sr.LastPlayed = r.LastPlayed.UnixMilli()
	}
	return sr
}

// newer reports whether a should replace b: it changed later, or (to make
// both sides of a sync agree) changed at the same time and sorts after it.
// Records from before change times were kept fall back to LastPlayed.
func newer(aTime, bTime int64, aKey, bKey string) bool {
	if aTime != bTime {
		return aTime > bTime
	}
	return aKey > bKey
}

func (r SyncRecord) when() int64 { return max(r.Updated, r.LastPlayed) }

func (r SyncRecord) canon() string {
	deref := func(p *int) string {
		if p == nil {
			return "-"
		}
		return fmt.Sprint(*p)
	}
	likes := "-"
	if r.Likes != nil {
		likes = fmt.Sprint(*r.Likes)
	}
	return fmt.Sprint(r.Played, r.PlayCount, r.IsFavorite, likes, r.Position, r.LastPlayed, deref(r.AudioStream), deref(r.SubtitleStream))
}

func (p SyncPlaylist) when() int64 { return max(p.Updated, p.Created) }

func (p SyncPlaylist) canon() string {
	var b strings.Builder
	b.WriteString(p.Name + "\x00" + p.MediaType)
	for _, e := range p.Entries {
		b.WriteString("\x00" + e.ID + ":" + e.Item)
	}
	return b.String()
}

// MergeStats counts what a merge changed.
type MergeStats struct {
	Records, Playlists, DeletedPlaylists int
}

// Merge brings another instance's state into this one. Per user and item,
// and per playlist, the most recent change wins, so merging two instances
// into each other leaves both with the same state. A playlist deleted on
// one side after its last change on the other is deleted on both.
func (s *Store) Merge(ctx context.Context, snap *Snapshot, resolve ResolveFunc) (MergeStats, error) {
	var st MergeStats
	if snap.Version != SnapshotVersion {
		return st, fmt.Errorf("userdata: snapshot version %d, want %d", snap.Version, SnapshotVersion)
	}
	if resolve == nil {
		resolve = func(_ context.Context, id string, _ []string) string { return id }
	}
	// Map items to local IDs before taking the write lock.
	for i := range snap.UserData {
		snap.UserData[i].Item = resolve(ctx, snap.UserData[i].Item, snap.UserData[i].Keys)
	}
	for i := range snap.Playlists {
		for j := range snap.Playlists[i].Entries {
			e := &snap.Playlists[i].Entries[j]
			e.Item = resolve(ctx, e.Item, e.Keys)
		}
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return st, err
	}
	defer tx.Rollback()
	// Write first: take the write lock now, so local edits during the merge
	// queue behind it rather than interleave.
	if _, err := tx.ExecContext(ctx, `DELETE FROM playlist_deletions WHERE 0`); err != nil {
		return st, err
	}

	for _, rr := range snap.UserData {
		var updated int64
		var user string
		local, err := scanRecord(scanFunc(func(dest ...any) error {
			return tx.QueryRowContext(ctx, `SELECT user_id, `+recordCols+`, updated FROM user_data WHERE user_id = ? AND item_id = ?`,
				rr.User, rr.Item).Scan(append(append([]any{&user}, dest...), &updated)...)
		}))
		switch {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return st, err
		default:
			lr := toSync(rr.User, local, updated)
			if !newer(rr.when(), lr.when(), rr.canon(), lr.canon()) {
				continue
			}
		}
		var likes, audio, sub any
		if rr.Likes != nil {
			likes = map[bool]int{true: 1, false: 0}[*rr.Likes]
		}
		if rr.AudioStream != nil {
			audio = *rr.AudioStream
		}
		if rr.SubtitleStream != nil {
			sub = *rr.SubtitleStream
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO user_data (user_id, `+recordCols+`, updated) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (user_id, item_id) DO UPDATE SET played = excluded.played, play_count = excluded.play_count,
				is_favorite = excluded.is_favorite, likes = excluded.likes, position = excluded.position,
				last_played = excluded.last_played, audio_stream = excluded.audio_stream, sub_stream = excluded.sub_stream,
				updated = excluded.updated`,
			rr.User, rr.Item, rr.Played, rr.PlayCount, rr.IsFavorite, likes, rr.Position, rr.LastPlayed, audio, sub, rr.when())
		if err != nil {
			return st, err
		}
		st.Records++
	}

	deletedAt := func(id string) (int64, error) {
		var at int64
		err := tx.QueryRowContext(ctx, `SELECT deleted FROM playlist_deletions WHERE id = ?`, id).Scan(&at)
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return at, err
	}
	localPlaylist := func(id string) (*SyncPlaylist, error) {
		p := &SyncPlaylist{ID: id}
		err := tx.QueryRowContext(ctx, `SELECT owner, name, media_type, created, updated FROM playlists WHERE id = ?`, id).
			Scan(&p.Owner, &p.Name, &p.MediaType, &p.Created, &p.Updated)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		es, err := playlistEntries(ctx, tx, id)
		for _, e := range es {
			p.Entries = append(p.Entries, SyncEntry{ID: e.ID, Item: e.ItemID})
		}
		return p, err
	}

	for _, rp := range snap.Playlists {
		gone, err := deletedAt(rp.ID)
		if err != nil {
			return st, err
		}
		if gone >= rp.when() {
			continue // deleted here after its last change there
		}
		lp, err := localPlaylist(rp.ID)
		if err != nil {
			return st, err
		}
		if lp != nil && !newer(rp.when(), lp.when(), rp.canon(), lp.canon()) {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO playlists (id, owner, name, media_type, created, updated) VALUES (?, ?, ?, ?, ?, ?)
			ON CONFLICT (id) DO UPDATE SET owner = excluded.owner, name = excluded.name, media_type = excluded.media_type,
				created = excluded.created, updated = excluded.updated`,
			rp.ID, rp.Owner, rp.Name, rp.MediaType, rp.Created, rp.when()); err != nil {
			return st, err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM playlist_entries WHERE playlist_id = ?`, rp.ID); err != nil {
			return st, err
		}
		for i, e := range rp.Entries {
			if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO playlist_entries (entry_id, playlist_id, item_id, position) VALUES (?, ?, ?, ?)`,
				e.ID, rp.ID, e.Item, i); err != nil {
				return st, err
			}
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM playlist_deletions WHERE id = ?`, rp.ID); err != nil {
			return st, err
		}
		st.Playlists++
	}

	for _, d := range snap.DeletedPlaylists {
		lp, err := localPlaylist(d.ID)
		if err != nil {
			return st, err
		}
		if lp != nil && lp.when() > d.Deleted {
			continue // changed here after it was deleted there: keep it
		}
		if lp != nil {
			st.DeletedPlaylists++
		}
		if err := deletePlaylist(ctx, tx, d.ID, d.Owner, d.Deleted); err != nil {
			return st, err
		}
	}
	return st, tx.Commit()
}
