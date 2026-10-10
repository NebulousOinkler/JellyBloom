package sources

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	// SampleSize is the length of each sampled block.
	SampleSize = 64 << 10
	// interior is the number of samples between the first and last blocks
	// (odd, so one sits at the middle).
	interior = 15
)

// Offsets returns where a file of size n is sampled: all of it, in
// SampleSize blocks, when it is small; otherwise its first and last blocks
// and 15 blocks spread evenly between them. Damage the samples miss is
// caught later by block hashes taken during playback.
func Offsets(size int64) []int64 {
	if size <= 0 {
		return nil
	}
	var out []int64
	if size <= (interior+2)*SampleSize {
		for off := int64(0); off < size; off += SampleSize {
			out = append(out, off)
		}
		return out
	}
	out = append(out, 0)
	span := size - SampleSize
	for i := int64(1); i <= interior; i++ {
		out = append(out, (span*i/(interior+1))&^4095)
	}
	return append(out, span)
}

// sampleLen is the length of the sample at off in a file of size n.
func sampleLen(size, off int64) int64 { return min(SampleSize, size-off) }

func sum64(b []byte) uint64 {
	h := sha256.Sum256(b)
	return binary.BigEndian.Uint64(h[:8])
}

// Fingerprint is a copy's sampled-content digest.
type Fingerprint struct {
	Size   int64
	Hashes []uint64 // one per Offsets(Size)
}

func (f Fingerprint) Equal(g Fingerprint) bool {
	if f.Size != g.Size || len(f.Hashes) != len(g.Hashes) {
		return false
	}
	for i := range f.Hashes {
		if f.Hashes[i] != g.Hashes[i] {
			return false
		}
	}
	return true
}

func (f Fingerprint) digest() []byte {
	b := make([]byte, 8*len(f.Hashes))
	for i, h := range f.Hashes {
		binary.BigEndian.PutUint64(b[8*i:], h)
	}
	return b
}

func parseDigest(size int64, b []byte) (Fingerprint, bool) {
	if len(b) != 8*len(Offsets(size)) {
		return Fingerprint{}, false
	}
	f := Fingerprint{Size: size, Hashes: make([]uint64, len(b)/8)}
	for i := range f.Hashes {
		f.Hashes[i] = binary.BigEndian.Uint64(b[8*i:])
	}
	return f, true
}

// errContent is a read that returned something other than the expected file.
var errContent = errors.New("upstream returned different content")

// streamPath is the upstream route that serves a copy's file as stored.
func streamPath(src Source) (string, url.Values) {
	return "/Videos/" + src.Item + "/stream", url.Values{"Static": {"true"}, "MediaSourceId": {src.Media}}
}

// readRange reads n bytes at off from a copy of the given size.
func (r *Registry) readRange(ctx context.Context, src Source, off, n, size int64) ([]byte, error) {
	if r.opts.Open == nil {
		return nil, errors.New("sources: no upstream access")
	}
	path, q := streamPath(src)
	hdr := http.Header{"Range": {fmt.Sprintf("bytes=%d-%d", off, off+n-1)}}
	start := r.now()
	resp, err := r.opts.Open(ctx, src.Upstream, http.MethodGet, path, q, hdr)
	if err != nil {
		r.upstreamFailed(src.Upstream, err)
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent {
		return nil, fmt.Errorf("%w: HTTP %d for a range", errContent, resp.StatusCode)
	}
	first, _, total, ok := parseContentRange(resp.Header.Get("Content-Range"))
	if !ok || first != off || total != size {
		return nil, fmt.Errorf("%w: Content-Range %q, want offset %d of %d bytes", errContent, resp.Header.Get("Content-Range"), off, size)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, n+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) != n {
		return nil, fmt.Errorf("%w: got %d bytes, want %d", errContent, len(b), n)
	}
	r.upstreamOK(src.Upstream)
	r.observeLatency(src.Upstream, r.now().Sub(start))
	return b, nil
}

// parseContentRange parses "bytes first-last/total".
func parseContentRange(s string) (first, last, total int64, ok bool) {
	s, found := strings.CutPrefix(strings.TrimSpace(s), "bytes ")
	if !found {
		return 0, 0, 0, false
	}
	rng, tot, found := strings.Cut(s, "/")
	if !found {
		return 0, 0, 0, false
	}
	a, b, found := strings.Cut(rng, "-")
	if !found {
		return 0, 0, 0, false
	}
	var err1, err2, err3 error
	first, err1 = strconv.ParseInt(a, 10, 64)
	last, err2 = strconv.ParseInt(b, 10, 64)
	total, err3 = strconv.ParseInt(tot, 10, 64)
	return first, last, total, err1 == nil && err2 == nil && err3 == nil && first <= last && last < total
}

// computeFingerprint samples a copy and stores its fingerprint.
func (r *Registry) computeFingerprint(ctx context.Context, c Copy) (Fingerprint, error) {
	f := Fingerprint{Size: c.Size}
	for _, off := range Offsets(c.Size) {
		b, err := r.readRange(ctx, c.Source, off, sampleLen(c.Size, off), c.Size)
		if err != nil {
			return Fingerprint{}, fmt.Errorf("sampling %s at %d: %w", c.Source, off, err)
		}
		f.Hashes = append(f.Hashes, sum64(b))
	}
	_, err := r.db.ExecContext(ctx, `INSERT INTO source_fingerprints (upstream, media, item, size, digest, checked) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (upstream, media) DO UPDATE SET item = excluded.item, size = excluded.size, digest = excluded.digest, checked = excluded.checked`,
		c.Upstream, c.Media, c.Item, c.Size, f.digest(), r.now().UnixMilli())
	return f, err
}

// fingerprints returns the stored fingerprints of copies that still match
// their copy's size, keyed by source.
func (r *Registry) fingerprints(ctx context.Context, copies []Copy) (map[key]storedFingerprint, error) {
	out := map[key]storedFingerprint{}
	for _, c := range copies {
		var size, checked int64
		var digest []byte
		err := r.db.QueryRowContext(ctx, `SELECT size, digest, checked FROM source_fingerprints WHERE upstream = ? AND media = ?`,
			c.Upstream, c.Media).Scan(&size, &digest, &checked)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return nil, err
			}
			continue // none yet
		}
		if f, ok := parseDigest(size, digest); ok && size == c.Size {
			out[c.key()] = storedFingerprint{f, time.UnixMilli(checked)}
		}
	}
	return out, nil
}

type storedFingerprint struct {
	Fingerprint
	checked time.Time
}

// fingerprintOf returns a source's stored fingerprint, if any.
func (r *Registry) fingerprintOf(ctx context.Context, s Source) (Fingerprint, bool) {
	var size int64
	var digest []byte
	if r.db.QueryRowContext(ctx, `SELECT size, digest FROM source_fingerprints WHERE upstream = ? AND media = ?`, s.Upstream, s.Media).Scan(&size, &digest) != nil {
		return Fingerprint{}, false
	}
	return parseDigest(size, digest)
}
