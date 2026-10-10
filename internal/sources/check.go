package sources

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
)

// Verdict states.
const (
	Truncated = "truncated" // a prefix of a longer copy of the same encode
	Damaged   = "damaged"   // same size as its peer, but some bytes differ and are bad
	Disputed  = "disputed"  // differs from its peer; no way to tell which is intact
)

// Verdict is what content checks concluded about a copy.
type Verdict struct {
	State   string
	Reason  string
	Peer    Source // the copy it was compared with (Item unset)
	Offset  int64  // evidence: where the copies differ
	Length  int64
	Size    int64 // the copy's size when checked; a different size voids it
	Checked int64 // unix ms
}

// Broken reports whether the copy must not be played when a good one exists.
func (v Verdict) Broken() bool { return v.State == Truncated || v.State == Damaged }

// verdict returns c's verdict, if it still applies to c's size.
func (r *Registry) verdict(c Copy) (Verdict, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	v, ok := r.verdicts[c.key()]
	return v, ok && v.Size == c.Size
}

func (r *Registry) broken(c Copy) bool {
	v, ok := r.verdict(c)
	return ok && v.Broken()
}

func (r *Registry) setVerdict(ctx context.Context, c Copy, v Verdict) {
	v.Size, v.Checked = c.Size, r.now().UnixMilli()
	r.mu.Lock()
	old, had := r.verdicts[c.key()]
	r.verdicts[c.key()] = v
	r.mu.Unlock()
	if !had || old.State != v.State || old.Size != v.Size {
		r.log.Warn("copy check", "source", c.Source.String(), "verdict", v.State, "reason", v.Reason)
	}
	_, err := r.db.ExecContext(ctx, `INSERT INTO source_verdicts (upstream, media, size, state, reason, peer_upstream, peer_media, offset, length, checked)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT (upstream, media) DO UPDATE SET size = excluded.size, state = excluded.state,
		reason = excluded.reason, peer_upstream = excluded.peer_upstream, peer_media = excluded.peer_media, offset = excluded.offset,
		length = excluded.length, checked = excluded.checked`,
		c.Upstream, c.Media, v.Size, v.State, v.Reason, v.Peer.Upstream, v.Peer.Media, v.Offset, v.Length, v.Checked)
	if err != nil {
		r.log.Error("saving copy verdict", "err", err)
	}
}

func (r *Registry) clearVerdict(ctx context.Context, k key) {
	r.mu.Lock()
	_, had := r.verdicts[k]
	delete(r.verdicts, k)
	r.mu.Unlock()
	if had {
		r.log.Info("copy check cleared", "source", k.up+"/"+k.media)
	}
	if _, err := r.db.ExecContext(ctx, `DELETE FROM source_verdicts WHERE upstream = ? AND media = ?`, k.up, k.media); err != nil {
		r.log.Error("clearing copy verdict", "err", err)
	}
}

// Grouping is how an item's copies split into versions.
type Grouping struct {
	Versions [][]Copy // each version's copies, byte-identical as far as checked
	Hidden   []Copy   // broken copies of an encode that has a good copy
}

// Group splits copies into versions. Copies are one version when their
// encode and size match and, where it was possible to check, their sampled
// content too. It fingerprints copies that grouping depends on (bounded by
// ctx), settles which of two disagreeing copies is damaged, and finds
// truncated copies. A broken copy is hidden when a good copy of its encode
// is among copies; otherwise it is kept, so it can still be played.
func (r *Registry) Group(ctx context.Context, copies []Copy) Grouping {
	parts := partition(copies)

	// Fingerprint where it matters: two or more copies that look identical,
	// or an encode present in more than one size (a truncation candidate).
	sizes := map[string]int{}
	for _, p := range parts {
		sizes[p[0].Key]++
	}
	var need []Copy
	for _, p := range parts {
		if p[0].Size <= 0 {
			continue
		}
		good := r.healthy(p)
		if len(good) >= 2 || sizes[p[0].Key] > 1 {
			need = append(need, good...)
		}
	}
	fps := r.ensure(ctx, need)

	// Same size, different content: settle which copies are damaged.
	for _, p := range parts {
		if groups := split(r.healthy(p), fps); len(groups) > 1 && !r.allDisputed(p) {
			r.resolve(ctx, groups, fps)
		}
	}
	// Same encode, smaller: a truncated copy of a longer one?
	for _, small := range parts {
		for _, big := range parts {
			if small[0].Key != big[0].Key || small[0].Size <= 0 || small[0].Size >= big[0].Size {
				continue
			}
			for _, s := range r.healthy(small) {
				for _, b := range r.healthy(big) {
					if r.truncation(ctx, s, b, fps) {
						break
					}
				}
			}
		}
	}

	var g Grouping
	good := map[string]bool{} // encode key -> has a good copy
	for _, c := range copies {
		if !r.broken(c) {
			good[c.Key] = true
		}
	}
	for _, p := range parts {
		var keep []Copy
		for _, c := range p {
			if r.broken(c) && good[c.Key] {
				g.Hidden = append(g.Hidden, c)
				continue
			}
			keep = append(keep, c)
		}
		for _, v := range split(keep, fps) {
			g.Versions = append(g.Versions, r.separateDisputed(v)...)
		}
	}
	return g
}

// separateDisputed gives each disputed copy its own version: it differs
// from its peer somewhere (perhaps only in a block found during playback),
// so the two must never be spliced together.
func (r *Registry) separateDisputed(v []Copy) [][]Copy {
	out := [][]Copy{{v[0]}}
	for _, c := range v[1:] {
		if d, ok := r.verdict(c); ok && d.State == Disputed {
			out = append(out, []Copy{c})
			continue
		}
		out[0] = append(out[0], c)
	}
	return out
}

func (r *Registry) allDisputed(copies []Copy) bool {
	for _, c := range copies {
		if v, ok := r.verdict(c); !ok || v.State != Disputed {
			return false
		}
	}
	return true
}

// partition groups copies by encode and size, in input order.
func partition(copies []Copy) [][]Copy {
	idx := map[string]int{}
	var out [][]Copy
	for _, c := range copies {
		k := c.Key + "|" + strconv.FormatInt(c.Size, 10)
		i, ok := idx[k]
		if !ok {
			i = len(out)
			idx[k] = i
			out = append(out, nil)
		}
		out[i] = append(out[i], c)
	}
	return out
}

// split groups same-size copies by fingerprint. Copies without one stay
// with the first group: they matched on metadata, the best evidence there is.
func split(copies []Copy, fps map[key]Fingerprint) [][]Copy {
	var out [][]Copy
	var digests []Fingerprint
	var unknown []Copy
	for _, c := range copies {
		f, ok := fps[c.key()]
		if !ok {
			unknown = append(unknown, c)
			continue
		}
		i := slices.IndexFunc(digests, f.Equal)
		if i < 0 {
			i = len(out)
			digests = append(digests, f)
			out = append(out, nil)
		}
		out[i] = append(out[i], c)
	}
	if len(unknown) > 0 {
		if len(out) == 0 {
			return [][]Copy{unknown}
		}
		out[0] = append(out[0], unknown...)
	}
	return out
}

func (r *Registry) healthy(copies []Copy) []Copy {
	var out []Copy
	for _, c := range copies {
		if !r.broken(c) {
			out = append(out, c)
		}
	}
	return out
}

// ensure returns fingerprints for copies, computing missing ones
// concurrently until ctx ends.
func (r *Registry) ensure(ctx context.Context, copies []Copy) map[key]Fingerprint {
	out := map[key]Fingerprint{}
	if len(copies) == 0 {
		return out
	}
	stored, err := r.fingerprints(ctx, copies)
	if err != nil {
		return out
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for _, c := range copies {
		if f, ok := stored[c.key()]; ok {
			out[c.key()] = f.Fingerprint
			continue
		}
		if r.opts.Open == nil {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()
			f, err := r.computeFingerprint(ctx, c)
			if err != nil {
				if ctx.Err() == nil {
					r.log.Warn("fingerprinting a copy failed", "source", c.Source.String(), "err", err)
				}
				return
			}
			mu.Lock()
			out[c.key()] = f
			mu.Unlock()
		}()
	}
	wg.Wait()
	return out
}

// zeroed reports whether a block is mostly zero bytes, as unreadable
// sectors, file holes, and failed copies read back. Encoders pad with 0xff
// (H.264 filler, MPEG-TS null packets), never zeros, so where two copies of
// a file differ, the mostly-zero one is the damaged one.
func zeroed(b []byte) bool {
	if len(b) < 512 {
		return false
	}
	n := 0
	for _, c := range b {
		if c == 0 {
			n++
		}
	}
	return n*2 >= len(b)
}

type digestGroup struct {
	copies []Copy
	fp     Fingerprint
}

// resolve settles which groups of same-size copies with different sampled
// content are damaged: a group whose differing block is zeroed where
// another's is not, else the minority when one group has more copies.
// Two equal groups with no such evidence are disputed. Groups that differ
// in most samples are different encodes, not damage, and are left alone.
func (r *Registry) resolve(ctx context.Context, groups [][]Copy, fps map[key]Fingerprint) {
	var gs []digestGroup
	for _, g := range groups {
		for _, c := range g {
			if f, ok := fps[c.key()]; ok {
				gs = append(gs, digestGroup{g, f})
				break
			}
		}
	}
	if len(gs) < 2 {
		return
	}
	slices.SortStableFunc(gs, func(a, b digestGroup) int { return len(b.copies) - len(a.copies) })
	ref := gs[0]
	related := []digestGroup{ref}
	for _, g := range gs[1:] {
		agree := 0
		for i := range ref.fp.Hashes {
			if ref.fp.Hashes[i] == g.fp.Hashes[i] {
				agree++
			}
		}
		if agree*2 >= len(ref.fp.Hashes) {
			related = append(related, g)
		}
	}
	if len(related) < 2 {
		return
	}

	// Read each differing sample from one copy per group.
	offsets := Offsets(ref.fp.Size)
	zeros := make([]int64, len(related)) // offset+1 of zeroed-block evidence
	firstDiff := int64(-1)
	for i, off := range offsets {
		same := true
		for _, g := range related[1:] {
			if g.fp.Hashes[i] != ref.fp.Hashes[i] {
				same = false
			}
		}
		if same {
			continue
		}
		if firstDiff < 0 {
			firstDiff = off
		}
		isZero := make([]bool, len(related))
		data := false
		for j, g := range related {
			b, err := r.readRange(ctx, g.copies[0].Source, off, sampleLen(ref.fp.Size, off), ref.fp.Size)
			if err != nil {
				return // settle it later
			}
			isZero[j] = zeroed(b)
			data = data || !isZero[j]
		}
		for j := range related {
			if isZero[j] && data && zeros[j] == 0 {
				zeros[j] = off + 1
			}
		}
	}

	n := sampleLen(ref.fp.Size, max(firstDiff, 0))
	var rest []int
	for j, g := range related {
		if zeros[j] == 0 {
			rest = append(rest, j)
			continue
		}
		off := zeros[j] - 1
		peer := related[(j+1)%len(related)].copies[0]
		for _, c := range g.copies {
			r.setVerdict(ctx, c, Verdict{State: Damaged, Peer: peer.Source, Offset: off, Length: sampleLen(ref.fp.Size, off),
				Reason: fmt.Sprintf("bytes at offset %d are zeroed where the copy on %s has data", off, peer.Upstream)})
		}
	}
	if len(rest) < 2 {
		return
	}
	// Majority: one group with more copies than any other is intact.
	if top := related[rest[0]]; len(top.copies) > len(related[rest[1]].copies) {
		for _, j := range rest[1:] {
			for _, c := range related[j].copies {
				r.setVerdict(ctx, c, Verdict{State: Damaged, Peer: top.copies[0].Source, Offset: firstDiff, Length: n,
					Reason: fmt.Sprintf("bytes at offset %d differ from %d other copies, which agree", firstDiff, len(top.copies))})
			}
		}
		return
	}
	for _, j := range rest {
		peer := related[rest[0]].copies[0]
		if j == rest[0] {
			peer = related[rest[1]].copies[0]
		}
		for _, c := range related[j].copies {
			r.setVerdict(ctx, c, Verdict{State: Disputed, Peer: peer.Source, Offset: firstDiff, Length: n,
				Reason: fmt.Sprintf("bytes at offset %d differ from the copy on %s; neither looks damaged", firstDiff, peer.Upstream)})
		}
	}
}

// truncation checks whether small is a prefix of big: its sampled blocks
// match big's bytes at the same offsets. It records a verdict when it is.
func (r *Registry) truncation(ctx context.Context, small, big Copy, fps map[key]Fingerprint) bool {
	sf, ok1 := fps[small.key()]
	bf, ok2 := fps[big.key()]
	if !ok1 || !ok2 {
		return false
	}
	pair := [2]key{small.key(), big.key()}
	r.mu.Lock()
	done := r.pairs[pair]
	r.mu.Unlock()
	if done {
		return false
	}
	noPrefix := func() bool {
		r.mu.Lock()
		r.pairs[pair] = true
		r.mu.Unlock()
		return false
	}
	offsets := Offsets(small.Size)
	for i, off := range offsets {
		n := sampleLen(small.Size, off)
		if i == 0 && n == sampleLen(big.Size, 0) {
			// Both fingerprints cover the same first block.
			if sf.Hashes[0] != bf.Hashes[0] {
				return noPrefix()
			}
			continue
		}
		b, err := r.readRange(ctx, big.Source, off, n, big.Size)
		if err != nil {
			return false // check again later
		}
		if sum64(b) != sf.Hashes[i] {
			return noPrefix()
		}
	}
	r.setVerdict(ctx, small, Verdict{State: Truncated, Peer: big.Source, Offset: small.Size, Length: big.Size - small.Size,
		Reason: fmt.Sprintf("ends after %d of %d bytes; what it has matches the copy on %s", small.Size, big.Size, big.Upstream)})
	return true
}

// EncodeKey identifies an encode apart from its size: container, runtime in
// seconds, and the codecs and shapes of its streams. Bitrate stands in for
// size when the size is unknown.
func EncodeKey(ms map[string]any) string {
	parts := []string{strings.ToLower(str(ms, "Container")), strconv.FormatInt(int64(num(ms["RunTimeTicks"]))/10_000_000, 10)}
	if num(ms["Size"]) == 0 {
		parts = append(parts, strconv.FormatInt(int64(num(ms["Bitrate"])), 10))
	}
	if list, ok := ms["MediaStreams"].([]any); ok {
		for _, e := range list {
			s, _ := e.(map[string]any)
			switch str(s, "Type") {
			case "Video":
				parts = append(parts, fmt.Sprintf("v:%s:%dx%d", strings.ToLower(str(s, "Codec")), int(num(s["Width"])), int(num(s["Height"]))))
			case "Audio":
				parts = append(parts, fmt.Sprintf("a:%s:%d", strings.ToLower(str(s, "Codec")), int(num(s["Channels"]))))
			}
		}
	}
	return strings.Join(parts, "|")
}

// Size returns a media source's file size (0 when unknown).
func Size(ms map[string]any) int64 { return int64(num(ms["Size"])) }

func num(v any) float64 {
	switch t := v.(type) {
	case json.Number:
		f, _ := t.Float64()
		return f
	case float64:
		return t
	case int:
		return float64(t)
	case int64:
		return float64(t)
	}
	return 0
}

func str(m map[string]any, k string) string {
	s, _ := m[k].(string)
	return s
}

// verdictAt returns the state of src's verdict for a copy of size bytes
// ("" for none).
func (r *Registry) verdictAt(src Source, size int64) string {
	v, _ := r.verdict(Copy{Source: src, Size: size})
	return v.State
}
