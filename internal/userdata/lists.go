package userdata

import (
	"cmp"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"slices"
	"time"
)

// ErrNotFound: no such playlist, collection, or entry (or not the caller's).
var ErrNotFound = errors.New("not found")

func newID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// Playlist is a user's ordered list of items.
type Playlist struct {
	ID, Owner, Name, MediaType string
	Created                    time.Time
	Entries                    []Entry
}

// Entry is one position in a playlist (the same item may appear twice).
type Entry struct {
	ID     string // Jellyfin's PlaylistItemId
	ItemID string
}

// ItemIDs lists the entries' items in order.
func (p *Playlist) ItemIDs() []string {
	ids := make([]string, len(p.Entries))
	for i, e := range p.Entries {
		ids[i] = e.ItemID
	}
	return ids
}

// Collection is a shared, unordered set of items (a Jellyfin BoxSet).
type Collection struct {
	ID, Name string
	Created  time.Time
	Items    []string
}

// CreatePlaylist creates a playlist for owner with initial items.
func (s *Store) CreatePlaylist(ctx context.Context, owner, name, mediaType string, items []string) (*Playlist, error) {
	p := &Playlist{ID: newID(), Owner: owner, Name: name, MediaType: mediaType, Created: s.now()}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO playlists VALUES (?, ?, ?, ?, ?)`, p.ID, owner, name, mediaType, p.Created.UnixMilli()); err != nil {
		return nil, err
	}
	if err := insertEntries(ctx, tx, p.ID, 0, items); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.Playlist(ctx, owner, p.ID)
}

func insertEntries(ctx context.Context, tx *sql.Tx, playlist string, from int, items []string) error {
	for i, it := range items {
		if _, err := tx.ExecContext(ctx, `INSERT INTO playlist_entries VALUES (?, ?, ?, ?)`, newID(), playlist, it, from+i); err != nil {
			return err
		}
	}
	return nil
}

// Playlist returns owner's playlist id with its entries.
func (s *Store) Playlist(ctx context.Context, owner, id string) (*Playlist, error) {
	p := &Playlist{ID: id}
	var created int64
	err := s.db.QueryRowContext(ctx, `SELECT owner, name, media_type, created FROM playlists WHERE id = ? AND owner = ?`, id, owner).
		Scan(&p.Owner, &p.Name, &p.MediaType, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	p.Created = time.UnixMilli(created)
	p.Entries, err = playlistEntries(ctx, s.db, id)
	return p, err
}

type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func playlistEntries(ctx context.Context, q querier, id string) ([]Entry, error) {
	rows, err := q.QueryContext(ctx, `SELECT entry_id, item_id FROM playlist_entries WHERE playlist_id = ? ORDER BY position`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Entry
	for rows.Next() {
		var e Entry
		if err := rows.Scan(&e.ID, &e.ItemID); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Playlists lists owner's playlists (entries included), by name.
func (s *Store) Playlists(ctx context.Context, owner string) ([]*Playlist, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM playlists WHERE owner = ? ORDER BY name COLLATE NOCASE, id`, owner)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	var out []*Playlist
	for _, id := range ids {
		p, err := s.Playlist(ctx, owner, id)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

// editPlaylist rewrites owner's playlist entries with fn.
func (s *Store) editPlaylist(ctx context.Context, owner, id string, fn func([]Entry) ([]Entry, error)) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Writing first takes the write lock now, so concurrent edits queue
	// instead of failing or overwriting each other; it also checks the owner.
	res, err := tx.ExecContext(ctx, `UPDATE playlists SET name = name WHERE id = ? AND owner = ?`, id, owner)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return cmp.Or(err, ErrNotFound)
	}
	current, err := playlistEntries(ctx, tx, id)
	if err != nil {
		return err
	}
	entries, err := fn(current)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM playlist_entries WHERE playlist_id = ?`, id); err != nil {
		return err
	}
	for i, e := range entries {
		if e.ID == "" {
			e.ID = newID()
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO playlist_entries VALUES (?, ?, ?, ?)`, e.ID, id, e.ItemID, i); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// AddToPlaylist appends items.
func (s *Store) AddToPlaylist(ctx context.Context, owner, id string, items []string) error {
	return s.editPlaylist(ctx, owner, id, func(es []Entry) ([]Entry, error) {
		for _, it := range items {
			es = append(es, Entry{ItemID: it})
		}
		return es, nil
	})
}

// RemoveFromPlaylist removes entries by entry ID.
func (s *Store) RemoveFromPlaylist(ctx context.Context, owner, id string, entryIDs []string) error {
	return s.editPlaylist(ctx, owner, id, func(es []Entry) ([]Entry, error) {
		return slices.DeleteFunc(es, func(e Entry) bool { return slices.Contains(entryIDs, e.ID) }), nil
	})
}

// MovePlaylistEntry moves an entry to newIndex (clamped).
func (s *Store) MovePlaylistEntry(ctx context.Context, owner, id, entryID string, newIndex int) error {
	return s.editPlaylist(ctx, owner, id, func(es []Entry) ([]Entry, error) {
		i := slices.IndexFunc(es, func(e Entry) bool { return e.ID == entryID })
		if i < 0 {
			return nil, ErrNotFound
		}
		e := es[i]
		es = slices.Delete(es, i, i+1)
		newIndex = max(0, min(newIndex, len(es)))
		return slices.Insert(es, newIndex, e), nil
	})
}

// UpdatePlaylist renames a playlist and, if items is non-nil, replaces its items.
func (s *Store) UpdatePlaylist(ctx context.Context, owner, id, name string, items []string) error {
	if _, err := s.Playlist(ctx, owner, id); err != nil {
		return err
	}
	if name != "" {
		if _, err := s.db.ExecContext(ctx, `UPDATE playlists SET name = ? WHERE id = ?`, name, id); err != nil {
			return err
		}
	}
	if items == nil {
		return nil
	}
	return s.editPlaylist(ctx, owner, id, func([]Entry) ([]Entry, error) {
		es := make([]Entry, len(items))
		for i, it := range items {
			es[i] = Entry{ItemID: it}
		}
		return es, nil
	})
}

// DeletePlaylist removes owner's playlist.
func (s *Store) DeletePlaylist(ctx context.Context, owner, id string) error {
	if _, err := s.Playlist(ctx, owner, id); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM playlist_entries WHERE playlist_id = ?`, id); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM playlists WHERE id = ?`, id)
	return err
}

// CreateCollection creates a shared collection.
func (s *Store) CreateCollection(ctx context.Context, name string, items []string) (*Collection, error) {
	c := &Collection{ID: newID(), Name: name, Created: s.now()}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO collections VALUES (?, ?, ?)`, c.ID, name, c.Created.UnixMilli()); err != nil {
		return nil, err
	}
	if err := s.AddToCollection(ctx, c.ID, items); err != nil {
		return nil, err
	}
	return s.Collection(ctx, c.ID)
}

// Collection returns a collection with its items (in the order added).
func (s *Store) Collection(ctx context.Context, id string) (*Collection, error) {
	c := &Collection{ID: id}
	var created int64
	err := s.db.QueryRowContext(ctx, `SELECT name, created FROM collections WHERE id = ?`, id).Scan(&c.Name, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	c.Created = time.UnixMilli(created)
	rows, err := s.db.QueryContext(ctx, `SELECT item_id FROM collection_items WHERE collection_id = ? AND hidden = 0 ORDER BY added, item_id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var it string
		rows.Scan(&it)
		c.Items = append(c.Items, it)
	}
	return c, rows.Err()
}

// Collections lists all collections by name.
func (s *Store) Collections(ctx context.Context) ([]*Collection, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM collections ORDER BY name COLLATE NOCASE, id`)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	var out []*Collection
	for _, id := range ids {
		c, err := s.Collection(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

// AddToCollection adds items (duplicates are ignored). For an upstream
// collection, id is its unified ID and the items are added on top of its
// upstream members.
func (s *Store) AddToCollection(ctx context.Context, id string, items []string) error {
	now := s.now().UnixMilli()
	for i, it := range items {
		if _, err := s.db.ExecContext(ctx, `INSERT INTO collection_items (collection_id, item_id, added) VALUES (?, ?, ?)
			ON CONFLICT (collection_id, item_id) DO UPDATE SET hidden = 0`, id, it, now+int64(i)); err != nil {
			return err
		}
	}
	return nil
}

// HideInCollection removes items from an upstream collection as JellyBloom
// shows it (the upstream is not changed).
func (s *Store) HideInCollection(ctx context.Context, id string, items []string) error {
	now := s.now().UnixMilli()
	for _, it := range items {
		if _, err := s.db.ExecContext(ctx, `INSERT INTO collection_items (collection_id, item_id, added, hidden) VALUES (?, ?, ?, 1)
			ON CONFLICT (collection_id, item_id) DO UPDATE SET hidden = 1`, id, it, now); err != nil {
			return err
		}
	}
	return nil
}

// CollectionEdits returns the items added to and hidden from an upstream
// collection.
func (s *Store) CollectionEdits(ctx context.Context, id string) (added, hidden []string, err error) {
	rows, err := s.db.QueryContext(ctx, `SELECT item_id, hidden FROM collection_items WHERE collection_id = ? ORDER BY added, item_id`, id)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var it string
		var h bool
		if err := rows.Scan(&it, &h); err != nil {
			return nil, nil, err
		}
		if h {
			hidden = append(hidden, it)
		} else {
			added = append(added, it)
		}
	}
	return added, hidden, rows.Err()
}

// RemoveFromCollection removes items.
func (s *Store) RemoveFromCollection(ctx context.Context, id string, items []string) error {
	for _, it := range items {
		if _, err := s.db.ExecContext(ctx, `DELETE FROM collection_items WHERE collection_id = ? AND item_id = ?`, id, it); err != nil {
			return err
		}
	}
	return nil
}

// DeleteCollection removes a collection.
func (s *Store) DeleteCollection(ctx context.Context, id string) error {
	if _, err := s.Collection(ctx, id); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM collection_items WHERE collection_id = ?`, id); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM collections WHERE id = ?`, id)
	return err
}
