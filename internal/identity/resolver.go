// Package identity matches the same item across upstreams, assigns stable
// unified IDs, and applies the configured set operation to decide which
// unified items are visible and which upstream copies serve them.
package identity

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/NebulousOinkler/JellyBloom/internal/index"
)

const schema = `
-- Sticky identity: every key ever seen keeps the unified ID it was given, so
-- IDs survive resyncs, new copies, and items being removed and re-added.
CREATE TABLE IF NOT EXISTS identity_keys (
	key        TEXT PRIMARY KEY,
	unified_id TEXT NOT NULL
);

-- When a new copy bridges two groups, the losing ID redirects to the winner.
CREATE TABLE IF NOT EXISTS identity_redirects (
	old_id TEXT PRIMARY KEY,
	new_id TEXT NOT NULL
);

-- Rebuilt wholesale on every Rebuild.
CREATE TABLE IF NOT EXISTS unified_items (
	unified_id TEXT PRIMARY KEY,
	type       TEXT NOT NULL,
	visible    INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS mappings (
	upstream   TEXT NOT NULL,
	real_id    TEXT NOT NULL,
	unified_id TEXT NOT NULL,
	is_source  INTEGER NOT NULL, -- copy participates under the set operation
	PRIMARY KEY (upstream, real_id)
);
CREATE INDEX IF NOT EXISTS mappings_unified ON mappings (unified_id);

-- IDs for upstream GUIDs that are not indexed items: minted on the fly, or
-- set as aliases for synthesized objects (merged libraries, genres, people).
-- An alias may cover several upstream objects (one merged library on two
-- servers); requests route to the lowest rank.
CREATE TABLE IF NOT EXISTS lazy_mappings (
	upstream   TEXT NOT NULL,
	real_id    TEXT NOT NULL,
	unified_id TEXT NOT NULL,
	rank       INTEGER NOT NULL DEFAULT 0,
	alias      INTEGER NOT NULL DEFAULT 0, -- 0 minted, 1 alias, 2 play session alias
	created    INTEGER NOT NULL DEFAULT 0, -- unix ms, set by SetAliases
	PRIMARY KEY (upstream, real_id)
);
CREATE INDEX IF NOT EXISTS lazy_mappings_unified ON lazy_mappings (unified_id, rank);
`

// Op is a set operation over upstreams.
type Op string

const (
	Union        Op = "union"
	Intersection Op = "intersection"
	Difference   Op = "difference"
)

// Mode configures the set operation. Upstreams lists every configured
// upstream; Base and Subtract are used by Difference only.
type Mode struct {
	Op        Op
	Upstreams []string
	Base      string
	Subtract  []string
}

// Resolver owns the unified ID space. It is safe for concurrent use.
type Resolver struct {
	idx    *index.Store
	db     *sql.DB
	mode   Mode
	log    *slog.Logger
	notify chan struct{}
	mu     sync.Mutex // serializes rebuilds

	onRebuild func(context.Context)
}

// OnRebuild registers fn to run after every successful rebuild (e.g. to
// refresh the catalog). Call before Run.
func (r *Resolver) OnRebuild(fn func(context.Context)) { r.onRebuild = fn }

// New creates a resolver over idx, creating its tables if needed.
func New(idx *index.Store, mode Mode, logger *slog.Logger) (*Resolver, error) {
	if logger == nil {
		logger = slog.Default()
	}
	if _, err := idx.DB().Exec(schema); err != nil {
		return nil, fmt.Errorf("identity: init schema: %w", err)
	}
	// Columns added after the first release.
	for _, col := range []string{"alias", "created"} {
		var has bool
		if err := idx.DB().QueryRow(`SELECT COUNT(*) > 0 FROM pragma_table_info('lazy_mappings') WHERE name = ?`, col).Scan(&has); err != nil {
			return nil, fmt.Errorf("identity: init schema: %w", err)
		}
		if !has {
			if _, err := idx.DB().Exec(`ALTER TABLE lazy_mappings ADD COLUMN ` + col + ` INTEGER NOT NULL DEFAULT 0`); err != nil {
				return nil, fmt.Errorf("identity: init schema: %w", err)
			}
		}
	}
	return &Resolver{idx: idx, db: idx.DB(), mode: mode, log: logger, notify: make(chan struct{}, 1)}, nil
}

// Notify requests a rebuild; requests coalesce. Wire it to index sync OnChange.
func (r *Resolver) Notify() {
	select {
	case r.notify <- struct{}{}:
	default:
	}
}

// Run rebuilds once at start and then after notifications, waiting debounce
// after the first notification so sync bursts produce one rebuild.
func (r *Resolver) Run(ctx context.Context, debounce time.Duration) error {
	r.Notify()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-r.notify:
		}
		if err := sleep(ctx, debounce); err != nil {
			return err
		}
		select { // absorb notifications that arrived during the debounce
		case <-r.notify:
		default:
		}
		if _, err := r.Rebuild(ctx); err != nil && ctx.Err() == nil {
			r.log.Error("identity rebuild failed", "err", err)
		}
		if n, err := r.PruneSessions(ctx, time.Now().Add(-SessionAliasTTL)); err != nil && ctx.Err() == nil {
			r.log.Error("pruning play session aliases failed", "err", err)
		} else if n > 0 {
			r.log.Debug("pruned play session aliases", "rows", n)
		}
	}
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Stats summarizes a rebuild.
type Stats struct {
	Items   int // upstream copies
	Unified int // unified items
	Merged  int // unified items with more than one copy
	Visible int // unified items visible under the set operation
}

// Rebuild recomputes identity and visibility from the current index.
func (r *Resolver) Rebuild(ctx context.Context) (Stats, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	start := time.Now()

	var items []*item
	err := r.idx.EachItem(ctx, func(up string, dto json.RawMessage) error {
		it, err := parseItem(up, dto)
		if err != nil {
			return fmt.Errorf("identity: %s: %w", up, err)
		}
		items = append(items, it)
		return nil
	})
	if err != nil {
		return Stats{}, err
	}
	sticky, err := r.loadSticky(ctx)
	if err != nil {
		return Stats{}, err
	}

	a := newAssigner(sticky)
	for tier := range tiers {
		var tierItems []*item
		for _, it := range items {
			rl, _ := ruleFor(it.Type)
			if rl.tier == tier {
				tierItems = append(tierItems, it)
			}
		}
		a.assignTier(tierItems)
	}

	vis := r.visibility(items, a.uid)
	stats := Stats{Items: len(items), Unified: len(vis.members)}
	for u, m := range vis.members {
		if len(m) > 1 {
			stats.Merged++
		}
		if vis.visible[u] {
			stats.Visible++
		}
	}
	if err := r.persist(ctx, a, vis); err != nil {
		return Stats{}, err
	}
	r.log.Info("identity rebuilt", "items", stats.Items, "unified", stats.Unified, "merged", stats.Merged,
		"visible", stats.Visible, "op", r.mode.Op, "took", time.Since(start).Round(time.Millisecond))
	if r.onRebuild != nil {
		r.onRebuild(ctx)
	}
	return stats, nil
}

// stickyState is the persisted identity memory.
type stickyState struct {
	keys      map[string]string // key -> unified ID
	redirects map[string]string // old ID -> new ID
}

func (s stickyState) follow(id string) string {
	for range 16 { // chains are short; bound guards against cycles
		next, ok := s.redirects[id]
		if !ok {
			return id
		}
		id = next
	}
	return id
}

func (r *Resolver) loadSticky(ctx context.Context) (stickyState, error) {
	s := stickyState{keys: map[string]string{}, redirects: map[string]string{}}
	for _, q := range []struct {
		sql string
		m   map[string]string
	}{
		{`SELECT key, unified_id FROM identity_keys`, s.keys},
		{`SELECT old_id, new_id FROM identity_redirects`, s.redirects},
	} {
		rows, err := r.db.QueryContext(ctx, q.sql)
		if err != nil {
			return s, err
		}
		for rows.Next() {
			var k, v string
			if err := rows.Scan(&k, &v); err != nil {
				rows.Close()
				return s, err
			}
			q.m[k] = v
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return s, err
		}
	}
	return s, nil
}

// assigner groups items and assigns unified IDs, tier by tier.
type assigner struct {
	sticky    stickyState
	uid       map[ref]string    // upstream copy -> unified ID
	claimed   map[string]bool   // unified IDs used in this rebuild
	keys      map[string]string // keys to persist
	redirects map[string]string // redirects to persist
}

func newAssigner(s stickyState) *assigner {
	return &assigner{sticky: s, uid: map[ref]string{}, claimed: map[string]bool{}, keys: map[string]string{}, redirects: map[string]string{}}
}

func (a *assigner) parentOf(it *item) func(string) string {
	return func(realID string) string {
		if realID == "" {
			return ""
		}
		return a.uid[ref{it.Upstream, realID}]
	}
}

// unionFind tracks groups and, per group, the strong provider IDs it holds so
// weak matches never merge items whose provider IDs disagree.
type unionFind struct {
	parent []int
	provs  []map[string]map[string]bool // root -> provider -> values
}

func (u *unionFind) find(i int) int {
	for u.parent[i] != i {
		u.parent[i] = u.parent[u.parent[i]]
		i = u.parent[i]
	}
	return i
}

func (u *unionFind) conflict(a, b int) bool {
	pa, pb := u.provs[a], u.provs[b]
	for p, va := range pa {
		vb, ok := pb[p]
		if !ok {
			continue
		}
		shared := false
		for v := range va {
			if vb[v] {
				shared = true
				break
			}
		}
		if !shared {
			return true
		}
	}
	return false
}

func (u *unionFind) union(a, b int) {
	ra, rb := u.find(a), u.find(b)
	if ra == rb {
		return
	}
	if rb < ra {
		ra, rb = rb, ra
	}
	u.parent[rb] = ra
	for p, vs := range u.provs[rb] {
		if u.provs[ra][p] == nil {
			u.provs[ra][p] = map[string]bool{}
		}
		for v := range vs {
			u.provs[ra][p][v] = true
		}
	}
	u.provs[rb] = nil
}

func (a *assigner) assignTier(items []*item) {
	n := len(items)
	strong := make([][]string, n)
	weak := make([]string, n)
	uf := &unionFind{parent: make([]int, n), provs: make([]map[string]map[string]bool, n)}
	for i, it := range items {
		strong[i], weak[i] = keys(it, a.parentOf(it))
		uf.parent[i] = i
		uf.provs[i] = map[string]map[string]bool{}
		for _, k := range strong[i] {
			if p := providerOf(k); p != "" {
				if uf.provs[i][p] == nil {
					uf.provs[i][p] = map[string]bool{}
				}
				uf.provs[i][p][k] = true
			}
		}
	}

	// Strong keys merge unconditionally.
	owner := map[string]int{}
	for i := range items {
		for _, k := range strong[i] {
			if j, ok := owner[k]; ok {
				uf.union(i, j)
			} else {
				owner[k] = i
			}
		}
	}
	// Weak keys merge only groups whose provider IDs don't conflict.
	buckets := map[string][]int{}
	var weakKeys []string
	for i, w := range weak {
		if w == "" {
			continue
		}
		if _, ok := buckets[w]; !ok {
			weakKeys = append(weakKeys, w)
		}
		buckets[w] = append(buckets[w], i)
	}
	slices.Sort(weakKeys)
	for _, w := range weakKeys {
		b := buckets[w]
		for _, i := range b[1:] {
			ri, r0 := uf.find(i), uf.find(b[0])
			if ri != r0 && !uf.conflict(ri, r0) {
				uf.union(ri, r0)
			}
		}
	}

	groups := map[int][]int{}
	for i := range items {
		r := uf.find(i)
		groups[r] = append(groups[r], i)
	}
	type group struct {
		canonical string
		keys      []string
		members   []int
	}
	var ordered []group
	for _, m := range groups {
		var g group
		g.members = m
		seen := map[string]bool{}
		var strongKeys, weakKeys []string
		for _, i := range m {
			for _, k := range strong[i] {
				if !seen[k] {
					seen[k] = true
					strongKeys = append(strongKeys, k)
				}
			}
			if w := weak[i]; w != "" && !seen[w] {
				seen[w] = true
				weakKeys = append(weakKeys, w)
			}
		}
		rl, _ := ruleFor(items[m[0]].Type)
		slices.SortFunc(strongKeys, func(x, y string) int {
			if d := rank(rl, x) - rank(rl, y); d != 0 {
				return d
			}
			return strings.Compare(x, y)
		})
		slices.Sort(weakKeys)
		g.keys = append(strongKeys, weakKeys...)
		g.canonical = g.keys[0]
		ordered = append(ordered, g)
	}
	slices.SortFunc(ordered, func(x, y group) int { return strings.Compare(x.canonical, y.canonical) })

	for _, g := range ordered {
		id := a.pickID(g.canonical, g.keys)
		for _, k := range g.keys {
			a.keys[k] = id
		}
		for _, i := range g.members {
			a.uid[items[i].ref()] = id
		}
	}
}

// rank orders strong keys by the rule's provider preference.
func rank(rl rule, key string) int {
	p := providerOf(key)
	for i, name := range rl.strong {
		if strings.EqualFold(name, p) {
			return i
		}
	}
	return len(rl.strong)
}

// pickID reuses a sticky ID from any of the group's keys, redirecting other
// sticky IDs the group bridges, or mints one from the canonical key.
func (a *assigner) pickID(canonical string, keys []string) string {
	var candidates []string
	for _, k := range keys {
		if id, ok := a.sticky.keys[k]; ok {
			id = a.sticky.follow(id)
			if !slices.Contains(candidates, id) {
				candidates = append(candidates, id)
			}
		}
	}
	slices.Sort(candidates)
	var id string
	for _, c := range candidates {
		if !a.claimed[c] {
			id = c
			break
		}
	}
	if id == "" {
		// No reusable ID: new item, or a split from a group that kept the ID.
		id = newID(canonical)
		for n := 2; a.claimed[id]; n++ {
			id = newID(canonical + "#" + strconv.Itoa(n))
		}
	}
	for _, c := range candidates {
		if c != id && !a.claimed[c] {
			a.redirects[c] = id // two groups merged; keep the old ID working
		}
	}
	a.claimed[id] = true
	return id
}

// visibility is the outcome of the set operation.
type visibility struct {
	typ     map[string]string
	members map[string][]ref
	visible map[string]bool
	source  map[ref]bool
}

// visibility applies the set operation. Leaves (unified items with no indexed
// children) are tested against the operation directly; containers (series,
// seasons, albums, artists, folders) are visible when any child is, so a
// difference or intersection never hides playable items behind their parents.
func (r *Resolver) visibility(items []*item, uid map[ref]string) visibility {
	v := visibility{typ: map[string]string{}, members: map[string][]ref{}, visible: map[string]bool{}, source: map[ref]bool{}}
	children := map[string][]string{}
	for _, it := range items {
		u := uid[it.ref()]
		if _, ok := v.typ[u]; !ok {
			v.typ[u] = it.Type
		}
		v.members[u] = append(v.members[u], it.ref())
		for _, p := range it.parents() {
			if pu := uid[ref{it.Upstream, p}]; pu != "" && pu != u {
				children[pu] = append(children[pu], u)
			}
		}
	}

	const (
		unvisited = iota
		visiting
		done
	)
	state := map[string]int{}
	var visit func(u string) bool
	visit = func(u string) bool {
		switch state[u] {
		case done:
			return v.visible[u]
		case visiting:
			return false // cycle; malformed hierarchy
		}
		state[u] = visiting
		ok := false
		if kids := children[u]; len(kids) > 0 {
			for _, k := range kids {
				if visit(k) { // visit every child so all get computed
					ok = true
				}
			}
		} else {
			ok = r.leafVisible(v.members[u])
		}
		v.visible[u] = ok
		state[u] = done
		return ok
	}
	for u := range v.members {
		visit(u)
	}

	for u, refs := range v.members {
		var src []ref
		if r.mode.Op == Difference {
			for _, rf := range refs {
				if rf.upstream == r.mode.Base {
					src = append(src, rf)
				}
			}
		}
		if len(src) == 0 {
			src = refs
		}
		if v.visible[u] {
			for _, rf := range src {
				v.source[rf] = true
			}
		}
	}
	return v
}

func (r *Resolver) leafVisible(refs []ref) bool {
	on := map[string]bool{}
	for _, rf := range refs {
		on[rf.upstream] = true
	}
	switch r.mode.Op {
	case Intersection:
		for _, up := range r.mode.Upstreams {
			if !on[up] {
				return false
			}
		}
		return true
	case Difference:
		if !on[r.mode.Base] {
			return false
		}
		for _, up := range r.mode.Subtract {
			if on[up] {
				return false
			}
		}
		return true
	default:
		return len(refs) > 0
	}
}

func (r *Resolver) persist(ctx context.Context, a *assigner, v visibility) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	for _, q := range []string{`DELETE FROM mappings`, `DELETE FROM unified_items`} {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	stmts := map[string]*sql.Stmt{}
	for name, q := range map[string]string{
		"key":      `INSERT INTO identity_keys (key, unified_id) VALUES (?, ?) ON CONFLICT (key) DO UPDATE SET unified_id = excluded.unified_id`,
		"redirect": `INSERT INTO identity_redirects (old_id, new_id) VALUES (?, ?) ON CONFLICT (old_id) DO UPDATE SET new_id = excluded.new_id`,
		"unified":  `INSERT INTO unified_items (unified_id, type, visible) VALUES (?, ?, ?)`,
		"mapping":  `INSERT INTO mappings (upstream, real_id, unified_id, is_source) VALUES (?, ?, ?, ?)`,
	} {
		st, err := tx.PrepareContext(ctx, q)
		if err != nil {
			return err
		}
		defer st.Close()
		stmts[name] = st
	}
	for k, id := range a.keys {
		if a.sticky.keys[k] == id {
			continue
		}
		if _, err := stmts["key"].ExecContext(ctx, k, id); err != nil {
			return err
		}
	}
	for old, id := range a.redirects {
		if _, err := stmts["redirect"].ExecContext(ctx, old, id); err != nil {
			return err
		}
	}
	for u, refs := range v.members {
		if _, err := stmts["unified"].ExecContext(ctx, u, v.typ[u], v.visible[u]); err != nil {
			return err
		}
		for _, rf := range refs {
			if _, err := stmts["mapping"].ExecContext(ctx, rf.upstream, rf.id, u, v.source[rf]); err != nil {
				return err
			}
		}
	}
	// An ID in use again must not also redirect elsewhere.
	if _, err := tx.ExecContext(ctx, `DELETE FROM identity_redirects WHERE old_id IN (SELECT unified_id FROM unified_items)`); err != nil {
		return err
	}
	return tx.Commit()
}

// Ref is one upstream copy of a unified item.
type Ref struct {
	Upstream string
	ID       string
	Source   bool // participates under the set operation
}

// Unified describes a unified item.
type Unified struct {
	ID      string
	Type    string // "" for lazy mappings
	Visible bool
	Lazy    bool // a non-indexed upstream GUID: minted, or an alias
	// Minted marks a lazy ID minted for one upstream's GUID that JellyBloom
	// knows nothing about (a cache tag, say), as opposed to an alias that
	// stands for objects JellyBloom set up (versions, play sessions, merged
	// libraries).
	Minted bool
	Refs   []Ref
}

// ErrNotFound is returned for unknown unified IDs.
var ErrNotFound = errors.New("identity: unknown unified id")

// NormalizeID converts a GUID in any spelling (dashed, braced, upper case) to
// Jellyfin's 32-hex lowercase form. Anything that is not a GUID is returned
// unchanged, so distinct non-GUID IDs can never collide.
func NormalizeID(id string) string {
	s := strings.TrimSuffix(strings.TrimPrefix(id, "{"), "}")
	if len(s) == 36 && s[8] == '-' && s[13] == '-' && s[18] == '-' && s[23] == '-' {
		s = s[:8] + s[9:13] + s[14:18] + s[19:23] + s[24:]
	}
	if len(s) != 32 {
		return id
	}
	for _, c := range s {
		if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F') {
			return id
		}
	}
	return strings.ToLower(s)
}

// Lookup resolves a unified ID (following merge redirects) to its copies.
func (r *Resolver) Lookup(ctx context.Context, id string) (*Unified, error) {
	id = NormalizeID(id)
	for range 16 {
		var next string
		err := r.db.QueryRowContext(ctx, `SELECT new_id FROM identity_redirects WHERE old_id = ?`, id).Scan(&next)
		if errors.Is(err, sql.ErrNoRows) {
			break
		}
		if err != nil {
			return nil, err
		}
		id = next
	}

	u := &Unified{ID: id}
	err := r.db.QueryRowContext(ctx, `SELECT type, visible FROM unified_items WHERE unified_id = ?`, id).Scan(&u.Type, &u.Visible)
	if errors.Is(err, sql.ErrNoRows) {
		rows, err := r.db.QueryContext(ctx, `SELECT upstream, real_id, alias FROM lazy_mappings WHERE unified_id = ? ORDER BY rank`, id)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		u.Minted = true
		for rows.Next() {
			rf := Ref{Source: true}
			var alias int
			if err := rows.Scan(&rf.Upstream, &rf.ID, &alias); err != nil {
				return nil, err
			}
			u.Minted = u.Minted && alias == 0
			u.Refs = append(u.Refs, rf)
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
		if len(u.Refs) == 0 {
			return nil, ErrNotFound
		}
		u.Lazy, u.Visible = true, true
		return u, nil
	}
	if err != nil {
		return nil, err
	}
	rows, err := r.db.QueryContext(ctx, `SELECT upstream, real_id, is_source FROM mappings WHERE unified_id = ? ORDER BY upstream, real_id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var rf Ref
		if err := rows.Scan(&rf.Upstream, &rf.ID, &rf.Source); err != nil {
			return nil, err
		}
		u.Refs = append(u.Refs, rf)
	}
	return u, rows.Err()
}

// UnifiedID returns the unified ID for an upstream GUID. Indexed items map to
// their unified item; any other GUID gets a stable lazy ID, recorded so it can
// be resolved back by Lookup.
func (r *Resolver) UnifiedID(ctx context.Context, upstream, realID string) (string, error) {
	realID = NormalizeID(realID)
	var id string
	err := r.db.QueryRowContext(ctx, `SELECT unified_id FROM mappings WHERE upstream = ? AND real_id = ?`, upstream, realID).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	err = r.db.QueryRowContext(ctx, `SELECT unified_id FROM lazy_mappings WHERE upstream = ? AND real_id = ?`,
		upstream, realID).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	id = newID("lazy:" + upstream + ":" + realID)
	_, err = r.db.ExecContext(ctx, `INSERT OR IGNORE INTO lazy_mappings (upstream, real_id, unified_id) VALUES (?, ?, ?)`, upstream, realID, id)
	return id, err
}

// Alias points a synthesized ID at the upstream objects it stands for, so
// requests naming it (e.g. for images) route to the first, and responses
// mentioning any of them show the synthesized ID.
type Alias struct {
	ID   string
	Refs []Ref // routing preference order
	// Session marks a play session's alias: it is pruned after
	// SessionAliasTTL, while other aliases last until replaced.
	Session bool
}

// SessionAliasTTL is how long play session aliases are kept.
const SessionAliasTTL = 7 * 24 * time.Hour

// SetAliases records aliases, replacing earlier mappings of the same
// synthesized IDs or upstream objects.
func (r *Resolver) SetAliases(ctx context.Context, aliases []Alias) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	delID, err := tx.PrepareContext(ctx, `DELETE FROM lazy_mappings WHERE unified_id = ?`)
	if err != nil {
		return err
	}
	defer delID.Close()
	upsert, err := tx.PrepareContext(ctx, `INSERT INTO lazy_mappings (upstream, real_id, unified_id, rank, alias, created) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (upstream, real_id) DO UPDATE SET unified_id = excluded.unified_id, rank = excluded.rank,
		alias = excluded.alias, created = excluded.created`)
	if err != nil {
		return err
	}
	defer upsert.Close()
	for _, a := range aliases {
		if _, err := delID.ExecContext(ctx, a.ID); err != nil {
			return err
		}
		kind := 1
		if a.Session {
			kind = 2
		}
		for i, rf := range a.Refs {
			if _, err := upsert.ExecContext(ctx, rf.Upstream, NormalizeID(rf.ID), a.ID, i, kind, time.Now().UnixMilli()); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

// PruneSessions forgets play session aliases set before cutoff.
func (r *Resolver) PruneSessions(ctx context.Context, cutoff time.Time) (int64, error) {
	res, err := r.db.ExecContext(ctx, `DELETE FROM lazy_mappings WHERE alias = 2 AND created < ?`, cutoff.UnixMilli())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// CopyDTO is one source copy of a unified item with its indexed DTO.
type CopyDTO struct {
	Upstream string
	ID       string // real item ID
	DTO      json.RawMessage
}

// fileTypes are the item types that have media files.
var fileTypes = []string{"Movie", "Episode", "Audio", "AudioBook", "MusicVideo", "Video", "Trailer"}

// EachFile calls fn for every visible item that has media files, with its
// source copies' DTOs (preferred upstream first is not implied).
func (r *Resolver) EachFile(ctx context.Context, fn func(id, typ string, copies []CopyDTO) error) error {
	q := `SELECT u.unified_id, u.type, m.upstream, m.real_id, i.dto FROM unified_items u
		JOIN mappings m ON m.unified_id = u.unified_id AND m.is_source = 1
		JOIN items i ON i.upstream = m.upstream AND i.real_id = m.real_id
		WHERE u.visible = 1 AND u.type IN (?` + strings.Repeat(", ?", len(fileTypes)-1) + `)
		ORDER BY u.unified_id, m.upstream, m.real_id`
	args := make([]any, len(fileTypes))
	for i, t := range fileTypes {
		args[i] = t
	}
	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	var id, typ string
	var copies []CopyDTO
	flush := func() error {
		if id == "" {
			return nil
		}
		return fn(id, typ, copies)
	}
	for rows.Next() {
		var uid, t string
		var c CopyDTO
		var dto []byte
		if err := rows.Scan(&uid, &t, &c.Upstream, &c.ID, &dto); err != nil {
			return err
		}
		c.DTO = dto
		if uid != id {
			if err := flush(); err != nil {
				return err
			}
			id, typ, copies = uid, t, nil
		}
		copies = append(copies, c)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return flush()
}

// KeysOf returns the match keys of unified item IDs (for moving user state
// to another JellyBloom, which may know an item under another ID).
func (r *Resolver) KeysOf(ctx context.Context, ids []string) (map[string][]string, error) {
	out := map[string][]string{}
	for start := 0; start < len(ids); start += 400 {
		chunk := ids[start:min(start+400, len(ids))]
		args := make([]any, len(chunk))
		for i, id := range chunk {
			args[i] = NormalizeID(id)
		}
		rows, err := r.db.QueryContext(ctx, `SELECT unified_id, key FROM identity_keys WHERE unified_id IN (?`+
			strings.Repeat(", ?", len(chunk)-1)+`) ORDER BY unified_id, key`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id, key string
			if err := rows.Scan(&id, &key); err != nil {
				rows.Close()
				return nil, err
			}
			out[id] = append(out[id], key)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// Resolve maps an item ID from another JellyBloom to this one's ID for the
// same item: the ID itself if this instance knows it (following merges),
// else the item one of its match keys belongs to, else the ID unchanged (the
// item may appear here later under it, since IDs derive from match keys).
func (r *Resolver) Resolve(ctx context.Context, id string, keys []string) string {
	id = NormalizeID(id)
	if u, err := r.Lookup(ctx, id); err == nil && !u.Lazy {
		return u.ID
	}
	for _, k := range keys {
		var uid string
		if r.db.QueryRowContext(ctx, `SELECT unified_id FROM identity_keys WHERE key = ?`, k).Scan(&uid) == nil {
			if u, err := r.Lookup(ctx, uid); err == nil && !u.Lazy {
				return u.ID
			}
			return uid
		}
	}
	return id
}
