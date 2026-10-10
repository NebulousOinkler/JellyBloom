package sources

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/NebulousOinkler/JellyBloom/internal/metrics"
)

var (
	relayBytes = metrics.Default.NewCounter("jellybloom_relay_bytes_total",
		"File bytes relayed to clients, by the upstream they came from.", "upstream")
	relayFailovers = metrics.Default.NewCounter("jellybloom_relay_failovers_total",
		"Relays that moved to another copy: before the response started, or mid-stream.", "when")
)

const (
	// BlockSize is the unit of the block hashes taken while relaying.
	BlockSize = 1 << 20
	// flushBlocks is how many block hashes are buffered before they are
	// saved and compared with identical copies.
	flushBlocks = 16
	copyBuffer  = 64 << 10
)

// Stream is one request for a file as stored: the copies that can serve
// it, and how to reach them.
type Stream struct {
	Sources []Source
	// OneVersion says Sources are one version as PlaybackInfo grouped them,
	// so any may answer. Otherwise only copies with the same content as the
	// best are used: a client stitches range requests for one URL together,
	// so every answer must come from the same file.
	OneVersion bool
	// The play session it belongs to (any may be empty).
	Session, User, Device, Item string
	// Open requests src's file with hdr; Relay sets Range.
	Open func(ctx context.Context, src Source, method string, hdr http.Header) (*http.Response, error)
}

// passHeaders are the response headers relayed to the client. Validators
// (ETag, Last-Modified) differ between upstreams for identical files, so
// they are dropped: a client must never pin a request to one upstream.
var passHeaders = []string{"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges", "Content-Disposition", "Cache-Control"}

// Relay serves a file from the best source. A source that errors or stalls
// before responding is skipped. A source that breaks off, stalls, or is
// found damaged mid-body is replaced by an identical copy, continuing from
// the same byte: the client sees one uninterrupted response.
func (r *Registry) Relay(w http.ResponseWriter, req *http.Request, st Stream) {
	ctx := req.Context()
	cands := r.Rank(st.Sources)
	if len(cands) == 0 {
		http.NotFound(w, req)
		return
	}
	// Stay on the session's upstream while it is as healthy as the best, so
	// a session's requests and reports keep going to one place.
	if active := r.Active(st.Session, st.User, st.Device, st.Item); active != "" {
		if i := slices.IndexFunc(cands, func(s Source) bool { return s.Upstream == active }); i > 0 && r.tierOf(cands[i]) <= r.tierOf(cands[0]) {
			c := cands[i]
			cands = append([]Source{c}, slices.Delete(cands, i, i+1)...)
		}
	}
	if !st.OneVersion {
		first := cands[0]
		cands = slices.DeleteFunc(cands, func(c Source) bool { return c != first && !r.sameContent(ctx, first, c) })
	}
	rng := req.Header.Get("Range")

	// Find a source that answers.
	var a *attempt
	status := http.StatusBadGateway
	for _, c := range cands {
		hdr := http.Header{}
		if rng != "" {
			hdr.Set("Range", rng)
		}
		next, err := r.open(ctx, st, c, req.Method, hdr)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			continue
		}
		code := next.resp.StatusCode
		if code == http.StatusOK || code == http.StatusPartialContent || code == http.StatusRequestedRangeNotSatisfiable {
			a = next
			break
		}
		next.close()
		relayFailovers.Inc("before_response")
		r.Failed(c, fmt.Sprintf("HTTP %d", code))
		if code == http.StatusNotFound && status == http.StatusBadGateway {
			status = http.StatusNotFound
		}
	}
	if a == nil {
		http.Error(w, http.StatusText(status), status)
		return
	}
	defer func() { a.close() }()

	h := w.Header()
	for _, k := range passHeaders {
		if v := a.resp.Header.Values(k); len(v) > 0 {
			h[k] = v
		}
	}
	w.WriteHeader(a.resp.StatusCode)
	if req.Method == http.MethodHead || a.resp.StatusCode == http.StatusRequestedRangeNotSatisfiable {
		r.done(st, a.src)
		return
	}

	// The absolute span this response covers, so a failover can request the
	// rest. Unknown for chunked bodies and multi-range responses.
	var first, last, total int64
	known := false
	switch {
	case a.resp.StatusCode == http.StatusPartialContent:
		first, last, total, known = parseContentRange(a.resp.Header.Get("Content-Range"))
	case a.resp.ContentLength >= 0:
		first, last, total, known = 0, a.resp.ContentLength-1, a.resp.ContentLength, true
	}

	pos := first
	bh := r.newBlockHasher(ctx, a.src, total, known)
	defer bh.flush()
	tried := map[key]bool{a.src.key(): true}
	for {
		reason, clientGone := r.copyBody(w, a, &pos, last, known, bh)
		if clientGone || ctx.Err() != nil {
			return
		}
		if reason == "" {
			r.Succeeded(a.src)
			r.done(st, a.src)
			return
		}
		r.Failed(a.src, reason)
		if !known {
			return
		}
		// Continue from pos with an identical copy.
		bh.abandonBlock()
		next := r.resume(ctx, st, cands, a.src, tried, pos, last, total)
		if next == nil {
			r.log.Warn("relay ended early: no identical copy to continue from", "source", a.src.String(), "at", pos, "of", total)
			return
		}
		relayFailovers.Inc("mid_stream")
		r.log.Info("relay failed over", "from", a.src.String(), "to", next.src.String(), "at", pos, "reason", reason)
		a.close()
		a = next
		bh.switchTo(a.src)
	}
}

func (r *Registry) tierOf(s Source) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.tier(s)
}

func (r *Registry) done(st Stream, src Source) {
	r.SetActive(st.Session, st.User, st.Device, st.Item, src.Upstream)
}

// attempt is one open upstream response with its stall guard.
type attempt struct {
	src     Source
	resp    *http.Response
	cancel  context.CancelFunc
	stalled atomic.Bool
	timer   *time.Timer
}

func (a *attempt) close() {
	a.timer.Stop()
	a.resp.Body.Close()
	a.cancel()
}

// open requests src with a stall guard on the response headers and body.
func (r *Registry) open(ctx context.Context, st Stream, src Source, method string, hdr http.Header) (*attempt, error) {
	actx, cancel := context.WithCancel(ctx)
	a := &attempt{src: src, cancel: cancel}
	a.timer = time.AfterFunc(r.opts.StallTimeout, func() { a.stalled.Store(true); cancel() })
	start := r.now()
	resp, err := st.Open(actx, src, method, hdr)
	a.timer.Stop()
	if err != nil {
		cancel()
		switch {
		case ctx.Err() != nil:
		case a.stalled.Load():
			r.Failed(src, fmt.Sprintf("no response within %s", r.opts.StallTimeout))
		default:
			r.upstreamFailed(src.Upstream, err)
		}
		return nil, err
	}
	r.upstreamOK(src.Upstream)
	r.observeLatency(src.Upstream, r.now().Sub(start))
	a.resp = resp
	return a, nil
}

// resume opens the rest of the span, pos..last, on an untried copy
// identical to from.
func (r *Registry) resume(ctx context.Context, st Stream, cands []Source, from Source, tried map[key]bool, pos, last, total int64) *attempt {
	for _, c := range r.Rank(cands) {
		if tried[c.key()] {
			continue
		}
		tried[c.key()] = true
		if !r.sameContent(ctx, from, c) {
			continue
		}
		hdr := http.Header{"Range": {fmt.Sprintf("bytes=%d-%d", pos, last)}}
		a, err := r.open(ctx, st, c, http.MethodGet, hdr)
		if err != nil {
			continue
		}
		f, l, t, ok := parseContentRange(a.resp.Header.Get("Content-Range"))
		if a.resp.StatusCode != http.StatusPartialContent || !ok || f != pos || l != last || t != total {
			r.Failed(c, fmt.Sprintf("answered a resume with HTTP %d %q", a.resp.StatusCode, a.resp.Header.Get("Content-Range")))
			a.close()
			continue
		}
		return a
	}
	return nil
}

// sameContent reports whether c can stand in for from byte for byte: the
// same size and sampled content (from may itself be damaged: c is then the
// good copy of the same file), with no verdict against c and no dispute.
func (r *Registry) sameContent(ctx context.Context, from, c Source) bool {
	f, ok1 := r.fingerprintOf(ctx, from)
	g, ok2 := r.fingerprintOf(ctx, c)
	return ok1 && ok2 && f.Equal(g) && r.verdictAt(c, g.Size) == "" && r.verdictAt(from, f.Size) != Disputed
}

// copyBody relays a's body to w until it ends, advancing pos. It returns
// why the upstream failed ("" for a complete body) or that the client left.
func (r *Registry) copyBody(w http.ResponseWriter, a *attempt, pos *int64, last int64, known bool, bh *blockHasher) (reason string, clientGone bool) {
	rc := http.NewResponseController(w)
	buf := make([]byte, copyBuffer)
	var body io.Reader = a.resp.Body
	if known {
		body = io.LimitReader(a.resp.Body, last+1-*pos)
	}
	var winBytes int64
	var winRead, winWrite time.Duration
	for {
		if bh.brokenNow() {
			return "found damaged while relaying", false
		}
		a.timer.Reset(r.opts.StallTimeout)
		t0 := time.Now()
		n, err := body.Read(buf)
		a.timer.Stop()
		winRead += time.Since(t0)
		if n > 0 {
			bh.write(*pos, buf[:n])
			t1 := time.Now()
			if _, werr := w.Write(buf[:n]); werr != nil {
				return "", true
			}
			rc.Flush()
			relayBytes.Add(float64(n), a.src.Upstream)
			winWrite += time.Since(t1)
			*pos += int64(n)
			winBytes += int64(n)
			if winBytes >= BlockSize {
				// Only windows where reading dominated measure the upstream;
				// otherwise the client was the bottleneck.
				if winRead > winWrite && winRead >= time.Millisecond {
					r.observeThroughput(a.src.Upstream, winBytes, winRead)
				}
				winBytes, winRead, winWrite = 0, 0, 0
			}
		}
		switch {
		case err == nil:
			continue
		case errors.Is(err, io.EOF):
			if known && *pos <= last {
				return fmt.Sprintf("body ended at byte %d of %d", *pos, last+1), false
			}
			return "", false
		case a.stalled.Load():
			return fmt.Sprintf("stalled for %s", r.opts.StallTimeout), false
		default:
			return "read failed: " + strings.TrimSpace(err.Error()), false
		}
	}
}

// blockHasher hashes the 1 MiB blocks a relay passes through, for copies
// that have identical peers, and compares them with the peers' hashes.
type blockHasher struct {
	r      *Registry
	ctx    context.Context
	src    Source
	size   int64
	on     bool
	wasBad bool // the source was already known broken (served as the only copy)
	h      hash.Hash
	block  int64 // block being hashed; -1 until a boundary is reached
	filled int64 // bytes of it hashed
	buf    []blockHash
	peers  []Source
}

type blockHash struct {
	src   Source
	block int64
	hash  uint64
}

func (r *Registry) newBlockHasher(ctx context.Context, src Source, size int64, known bool) *blockHasher {
	bh := &blockHasher{r: r, ctx: context.WithoutCancel(ctx), src: src, size: size, block: -1, h: sha256.New()}
	if known && size > 0 {
		bh.peers = r.peerSources(ctx, src)
		bh.on = len(bh.peers) > 0
		bh.wasBad = r.broken(Copy{Source: src, Size: size})
	}
	return bh
}

func (bh *blockHasher) write(pos int64, p []byte) {
	if !bh.on {
		return
	}
	for len(p) > 0 {
		if bh.block < 0 {
			// Skip to the next block boundary: only whole blocks are hashed.
			skip := (BlockSize - pos%BlockSize) % BlockSize
			if skip >= int64(len(p)) {
				return
			}
			p, pos = p[skip:], pos+skip
			bh.block, bh.filled = pos/BlockSize, 0
			bh.h.Reset()
		}
		end := min(BlockSize, bh.size-bh.block*BlockSize)
		n := min(int64(len(p)), end-bh.filled)
		bh.h.Write(p[:n])
		bh.filled += n
		p, pos = p[n:], pos+n
		if bh.filled == end {
			bh.buf = append(bh.buf, blockHash{bh.src, bh.block, binary.BigEndian.Uint64(bh.h.Sum(nil)[:8])})
			bh.block, bh.filled = bh.block+1, 0
			bh.h.Reset()
			if bh.block*BlockSize >= bh.size {
				bh.block = -1
			}
			if len(bh.buf) >= flushBlocks {
				bh.flush()
			}
		}
	}
}

// abandonBlock drops a block that a failover will finish from another copy.
func (bh *blockHasher) abandonBlock() {
	bh.block, bh.filled = -1, 0
	bh.h.Reset()
}

func (bh *blockHasher) switchTo(src Source) {
	bh.flush()
	bh.src = src
	bh.peers = bh.r.peerSources(bh.ctx, src)
	bh.on = bh.on && len(bh.peers) > 0
	bh.wasBad = bh.r.broken(Copy{Source: src, Size: bh.size})
}

// brokenNow reports whether the source being relayed has been found broken
// since the relay began (by a block comparison it triggered, or another
// check), so the relay should move to an identical copy.
func (bh *blockHasher) brokenNow() bool {
	return bh.on && !bh.wasBad && bh.r.broken(Copy{Source: bh.src, Size: bh.size})
}

// flush saves buffered block hashes and compares them with the peers'.
func (bh *blockHasher) flush() {
	if len(bh.buf) == 0 {
		return
	}
	hashes := bh.buf
	bh.buf = nil
	bh.r.recordBlocks(bh.ctx, hashes, bh.peers, bh.size)
}

// peerSources returns the copies with the same fingerprint as s.
func (r *Registry) peerSources(ctx context.Context, s Source) []Source {
	f, ok := r.fingerprintOf(ctx, s)
	if !ok {
		return nil
	}
	rows, err := r.db.QueryContext(ctx, `SELECT upstream, media, item FROM source_fingerprints WHERE size = ? AND digest = ? AND NOT (upstream = ? AND media = ?)`,
		f.Size, f.digest(), s.Upstream, s.Media)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []Source
	for rows.Next() {
		var p Source
		if rows.Scan(&p.Upstream, &p.Media, &p.Item) == nil {
			out = append(out, p)
		}
	}
	return out
}

// recordBlocks stores block hashes and starts settling any block on which a
// peer disagrees.
func (r *Registry) recordBlocks(ctx context.Context, hashes []blockHash, peers []Source, size int64) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return
	}
	defer tx.Rollback()
	for _, b := range hashes {
		if _, err := tx.ExecContext(ctx, `INSERT INTO source_blocks (upstream, media, block, size, hash) VALUES (?, ?, ?, ?, ?)
			ON CONFLICT (upstream, media, block) DO UPDATE SET size = excluded.size, hash = excluded.hash`,
			b.src.Upstream, b.src.Media, b.block, size, int64(b.hash)); err != nil {
			return
		}
	}
	if tx.Commit() != nil {
		return
	}
	for _, b := range hashes {
		for _, p := range peers {
			var h int64
			err := r.db.QueryRowContext(ctx, `SELECT hash FROM source_blocks WHERE upstream = ? AND media = ? AND block = ? AND size = ?`,
				p.Upstream, p.Media, b.block, size).Scan(&h)
			if err == nil && uint64(h) != b.hash {
				go r.settleBlock(ctx, append([]Source{b.src}, peers...), b.block, size)
				break
			}
		}
	}
}

// settleBlock rereads a block from every identical copy and decides which
// are damaged, as content checks do for samples: zeroed blocks first, then
// the majority; two disagreeing copies with no evidence are disputed.
func (r *Registry) settleBlock(ctx context.Context, srcs []Source, block, size int64) {
	r.mu.Lock()
	k := key{fmt.Sprint(block, size), srcs[0].Media}
	if r.checking[k] {
		r.mu.Unlock()
		return
	}
	r.checking[k] = true
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		delete(r.checking, k)
		r.mu.Unlock()
	}()

	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	off := block * BlockSize
	n := min(BlockSize, size-off)
	type group struct {
		hash   uint64
		zero   bool
		copies []Copy
	}
	var groups []*group
	for _, s := range srcs {
		b, err := r.readRange(ctx, s, off, n, size)
		if err != nil {
			continue
		}
		h := sum64(b)
		r.db.ExecContext(ctx, `UPDATE source_blocks SET hash = ? WHERE upstream = ? AND media = ? AND block = ?`, int64(h), s.Upstream, s.Media, block)
		i := slices.IndexFunc(groups, func(g *group) bool { return g.hash == h })
		if i < 0 {
			groups = append(groups, &group{hash: h, zero: zeroed(b)})
			i = len(groups) - 1
		}
		groups[i].copies = append(groups[i].copies, Copy{Source: s, Size: size})
	}
	if len(groups) < 2 {
		return // they agree now: the earlier mismatch was in transit
	}
	slices.SortStableFunc(groups, func(a, b *group) int { return len(b.copies) - len(a.copies) })
	var rest []*group
	for _, g := range groups {
		if g.zero && slices.ContainsFunc(groups, func(o *group) bool { return !o.zero }) {
			peer := groups[slices.IndexFunc(groups, func(o *group) bool { return !o.zero })].copies[0]
			for _, c := range g.copies {
				r.setVerdict(ctx, c, Verdict{State: Damaged, Peer: peer.Source, Offset: off, Length: n,
					Reason: fmt.Sprintf("block at offset %d is zeroed where the copy on %s has data (found during playback)", off, peer.Upstream)})
			}
			continue
		}
		rest = append(rest, g)
	}
	if len(rest) < 2 {
		return
	}
	if len(rest[0].copies) > len(rest[1].copies) {
		for _, g := range rest[1:] {
			for _, c := range g.copies {
				r.setVerdict(ctx, c, Verdict{State: Damaged, Peer: rest[0].copies[0].Source, Offset: off, Length: n,
					Reason: fmt.Sprintf("block at offset %d differs from %d other copies, which agree (found during playback)", off, len(rest[0].copies))})
			}
		}
		return
	}
	for i, g := range rest {
		peer := rest[(i+1)%len(rest)].copies[0]
		for _, c := range g.copies {
			r.setVerdict(ctx, c, Verdict{State: Disputed, Peer: peer.Source, Offset: off, Length: n,
				Reason: fmt.Sprintf("block at offset %d differs from the copy on %s; neither looks damaged (found during playback)", off, peer.Upstream)})
		}
	}
}
