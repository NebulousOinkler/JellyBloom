package sources

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	pingEvery     = 30 * time.Second
	verifyEvery   = 6 * time.Hour
	verifyDelay   = 30 * time.Second       // after start, and after a Kick
	verifyPause   = 200 * time.Millisecond // between items, to stay light
	refreshAfter  = 30 * 24 * time.Hour    // resample fingerprints this old
	recheckAfter  = 24 * time.Hour         // re-read evidence of verdicts this old
	itemCheckTime = 2 * time.Minute
)

// Run pings upstreams, re-checks quarantined sources when their backoff
// ends, expires idle play sessions, and verifies copies in the background:
// every item with copies on more than one upstream is fingerprinted, so
// grouping and failover rarely wait for it. It returns when ctx ends.
func (r *Registry) Run(ctx context.Context) {
	if r.opts.Open == nil {
		return
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		r.verifyLoop(ctx)
	}()
	t := time.NewTicker(pingEvery)
	defer t.Stop()
	for {
		r.ping(ctx)
		r.recheckQuarantined(ctx)
		r.expireSessions()
		select {
		case <-ctx.Done():
			wg.Wait()
			return
		case <-t.C:
		}
	}
}

// ping measures each upstream's latency and reachability.
func (r *Registry) ping(ctx context.Context) {
	var wg sync.WaitGroup
	for _, up := range r.opts.Upstreams {
		wg.Add(1)
		go func() {
			defer wg.Done()
			pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			start := r.now()
			resp, err := r.opts.Open(pctx, up, http.MethodGet, "/System/Ping", nil, nil)
			if err == nil {
				resp.Body.Close()
				if resp.StatusCode >= 500 {
					err = &statusError{resp.StatusCode}
				}
			}
			if ctx.Err() != nil {
				return
			}
			if err != nil {
				r.mu.Lock()
				s := r.statsOf(up)
				s.pingFails++
				fails := s.pingFails
				r.mu.Unlock()
				if fails >= 2 {
					r.upstreamFailed(up, err)
				}
				return
			}
			r.upstreamOK(up)
			r.observeLatency(up, r.now().Sub(start))
		}()
	}
	wg.Wait()
}

type statusError struct{ code int }

func (e *statusError) Error() string { return http.StatusText(e.code) }

// recheckQuarantined probes sources whose quarantine has ended with a
// one-byte read: success ends the quarantine, failure doubles it.
func (r *Registry) recheckQuarantined(ctx context.Context) {
	r.mu.Lock()
	var due []Source
	for k, h := range r.health {
		if !h.until.IsZero() && !h.until.After(r.now()) {
			due = append(due, Source{Upstream: k.up, Item: h.item, Media: k.media})
		}
	}
	r.mu.Unlock()
	for _, src := range due {
		if r.Down(src.Upstream) {
			continue // wait for the upstream to come back
		}
		pctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		path, q := streamPath(src)
		resp, err := r.opts.Open(pctx, src.Upstream, http.MethodGet, path, q, http.Header{"Range": {"bytes=0-0"}})
		if err == nil {
			resp.Body.Close()
		}
		cancel()
		switch {
		case ctx.Err() != nil:
			return
		case err != nil:
			r.upstreamFailed(src.Upstream, err)
		case resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusPartialContent:
			r.Succeeded(src)
		default:
			r.Failed(src, "re-check: "+http.StatusText(resp.StatusCode))
		}
	}
}

func (r *Registry) verifyLoop(ctx context.Context) {
	wait := verifyDelay
	for {
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-r.kick:
			timer.Stop()
			if sleep(ctx, verifyDelay) != nil {
				return
			}
		case <-timer.C:
		}
		r.Verify(ctx)
		wait = verifyEvery
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

// Verify checks every item with more than one copy: fingerprints what
// grouping needs (resampling old fingerprints), settles damaged and
// truncated copies, re-reads the evidence of old verdicts, and forgets
// state about copies that no longer exist.
func (r *Registry) Verify(ctx context.Context) {
	if r.opts.Library == nil || r.opts.Open == nil {
		return
	}
	var items [][]Copy
	seen := map[key]bool{}
	err := r.opts.Library.EachItem(ctx, func(it LibraryItem) error {
		copies := Copies(it)
		for _, c := range copies {
			seen[c.key()] = true
		}
		if len(copies) > 1 {
			items = append(items, copies)
		}
		return nil
	})
	if err != nil {
		if ctx.Err() == nil {
			r.log.Error("listing copies to verify", "err", err)
		}
		return
	}
	r.prune(ctx, seen)
	checked := 0
	for _, copies := range items {
		if ctx.Err() != nil {
			return
		}
		r.voidStale(ctx, copies)
		if r.recheckVerdicts(ctx, copies) {
			checked++
		}
		ictx, cancel := context.WithTimeout(ctx, itemCheckTime)
		r.Group(ictx, copies)
		cancel()
		if sleep(ctx, verifyPause) != nil {
			return
		}
	}
	r.log.Info("copies verified", "items", len(items), "verdicts rechecked", checked)
}

// voidStale drops fingerprints older than refreshAfter (so Group resamples
// them) and verdicts recorded for a different size than the copy has now.
func (r *Registry) voidStale(ctx context.Context, copies []Copy) {
	cutoff := r.now().Add(-refreshAfter).UnixMilli()
	for _, c := range copies {
		r.db.ExecContext(ctx, `DELETE FROM source_fingerprints WHERE upstream = ? AND media = ? AND (checked < ? OR size != ?)`,
			c.Upstream, c.Media, cutoff, c.Size)
		r.mu.Lock()
		v, ok := r.verdicts[c.key()]
		r.mu.Unlock()
		if ok && v.Size != c.Size {
			r.clearVerdict(ctx, c.key())
		}
	}
}

// recheckVerdicts revisits verdicts older than recheckAfter. A disputed
// verdict is dropped, so grouping decides afresh (a third copy may have
// appeared). A damaged copy's evidence is re-read: if the copy now matches
// its peer there (the file was replaced), the verdict is dropped and the
// copy resampled. It reports whether it revisited any.
func (r *Registry) recheckVerdicts(ctx context.Context, copies []Copy) bool {
	byKey := map[key]Copy{}
	for _, c := range copies {
		byKey[c.key()] = c
	}
	read := false
	for _, c := range copies {
		v, ok := r.verdict(c)
		if !ok || v.State == Truncated || r.now().UnixMilli()-v.Checked < recheckAfter.Milliseconds() {
			continue
		}
		if v.State == Disputed {
			read = true
			r.clearVerdict(ctx, c.key())
			continue
		}
		peer, ok := byKey[v.Peer.key()]
		if !ok {
			continue
		}
		read = true
		a, err1 := r.readRange(ctx, c.Source, v.Offset, v.Length, c.Size)
		b, err2 := r.readRange(ctx, peer.Source, v.Offset, v.Length, peer.Size)
		switch {
		case err1 != nil || err2 != nil:
		case bytes.Equal(a, b):
			r.clearVerdict(ctx, c.key())
			r.db.ExecContext(ctx, `DELETE FROM source_fingerprints WHERE upstream = ? AND media = ?`, c.Upstream, c.Media)
		default:
			v.Checked = r.now().UnixMilli()
			r.setVerdict(ctx, c, v)
		}
	}
	return read
}

// prune forgets fingerprints, block hashes, verdicts, and health of copies
// that are no longer in the library.
func (r *Registry) prune(ctx context.Context, seen map[key]bool) {
	for _, table := range []string{"source_fingerprints", "source_verdicts", "source_health"} {
		rows, err := r.db.QueryContext(ctx, `SELECT upstream, media FROM `+table)
		if err != nil {
			return
		}
		var gone []key
		for rows.Next() {
			var k key
			if rows.Scan(&k.up, &k.media) == nil && !seen[k] {
				gone = append(gone, k)
			}
		}
		rows.Close()
		for _, k := range gone {
			r.db.ExecContext(ctx, `DELETE FROM `+table+` WHERE upstream = ? AND media = ?`, k.up, k.media)
			if table == "source_fingerprints" {
				r.db.ExecContext(ctx, `DELETE FROM source_blocks WHERE upstream = ? AND media = ?`, k.up, k.media)
			}
			r.mu.Lock()
			switch table {
			case "source_verdicts":
				delete(r.verdicts, k)
			case "source_health":
				delete(r.health, k)
			}
			r.mu.Unlock()
		}
	}
}

// Copies returns an item's copies: every media source of every upstream
// DTO, as indexed.
func Copies(it LibraryItem) []Copy {
	var out []Copy
	for _, lc := range it.Copies {
		dec := json.NewDecoder(bytes.NewReader(lc.DTO))
		dec.UseNumber()
		var dto struct{ MediaSources []map[string]any }
		if dec.Decode(&dto) != nil {
			continue
		}
		for _, ms := range dto.MediaSources {
			id := normID(str(ms, "Id"))
			if id == "" {
				id = normID(lc.Item)
			}
			out = append(out, Copy{Source: Source{Upstream: lc.Upstream, Item: normID(lc.Item), Media: id}, Size: Size(ms), Key: EncodeKey(ms)})
		}
	}
	return out
}

// normID writes a GUID as Jellyfin's 32 lowercase hex digits.
func normID(id string) string {
	return strings.ToLower(strings.ReplaceAll(strings.Trim(id, "{}"), "-", ""))
}
