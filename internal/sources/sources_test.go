package sources

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// fakeUpstream serves files by media source ID with Range support, and can
// fail on demand.
type fakeUpstream struct {
	srv *httptest.Server

	mu         sync.Mutex
	files      map[string][]byte // media source ID -> content
	cutAfter   int64             // > 0: end each body after this many bytes
	stallAfter int64             // > 0: stop sending after this many bytes
	status     int               // != 0: answer media requests with this status
	ranges     []string          // Range headers received
	slow       time.Duration     // delay per 64 KiB sent
}

func newFake(t *testing.T) *fakeUpstream {
	f := &fakeUpstream{files: map[string][]byte{}}
	f.srv = httptest.NewServer(f)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeUpstream) set(fn func(f *fakeUpstream)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func (f *fakeUpstream) requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.ranges...)
}

func (f *fakeUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/System/Ping" {
		w.Write([]byte(`"Jellyfin Server"`))
		return
	}
	f.mu.Lock()
	var ms string
	for k, v := range r.URL.Query() {
		if strings.EqualFold(k, "MediaSourceId") { // Jellyfin binds parameters case-insensitively
			ms = v[0]
		}
	}
	data, ok := f.files[ms]
	cut, stall, status, slow := f.cutAfter, f.stallAfter, f.status, f.slow
	f.ranges = append(f.ranges, r.Header.Get("Range"))
	f.mu.Unlock()
	if status != 0 {
		http.Error(w, "injected", status)
		return
	}
	if !ok {
		http.NotFound(w, r)
		return
	}
	start, end := int64(0), int64(len(data))-1
	code := http.StatusOK
	if rng := r.Header.Get("Range"); rng != "" {
		spec := strings.TrimPrefix(rng, "bytes=")
		a, b, _ := strings.Cut(spec, "-")
		switch {
		case a == "":
			n, _ := strconv.ParseInt(b, 10, 64)
			start = int64(len(data)) - n
		default:
			start, _ = strconv.ParseInt(a, 10, 64)
			if b != "" {
				end, _ = strconv.ParseInt(b, 10, 64)
			}
		}
		end = min(end, int64(len(data))-1)
		if start > end {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", len(data)))
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		code = http.StatusPartialContent
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
	}
	w.Header().Set("Content-Type", "video/x-matroska")
	w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
	w.Header().Set("Last-Modified", "Sat, 10 Oct 2026 07:19:59 GMT")
	w.Header().Set("Accept-Ranges", "bytes")
	w.WriteHeader(code)
	body := data[start : end+1]
	sent := int64(0)
	for len(body) > 0 {
		n := min(int64(len(body)), 64<<10)
		if cut > 0 && sent+n > cut {
			w.Write(body[:cut-sent])
			return // short body: the connection closes
		}
		if stall > 0 && sent+n > stall {
			w.Write(body[:stall-sent])
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		}
		if _, err := w.Write(body[:n]); err != nil {
			return
		}
		if slow > 0 {
			w.(http.Flusher).Flush()
			time.Sleep(slow)
		}
		body, sent = body[n:], sent+n
	}
}

type rig struct {
	t   *testing.T
	ups map[string]*fakeUpstream
	reg *Registry
	db  *sql.DB
	now time.Time
	mu  sync.Mutex
}

func newRig(t *testing.T, names ...string) *rig {
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "t.db")+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	g := &rig{t: t, ups: map[string]*fakeUpstream{}, db: db, now: time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)}
	for _, n := range names {
		g.ups[n] = newFake(t)
	}
	g.reg = g.open()
	return g
}

// open creates a registry over the rig's database (again, to test reloads).
func (g *rig) open() *Registry {
	reg, err := New(g.db, Options{
		Upstreams: sortedNames(g.ups), Open: g.opener(), StallTimeout: 300 * time.Millisecond,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now: func() time.Time {
			g.mu.Lock()
			defer g.mu.Unlock()
			return g.now
		},
	})
	if err != nil {
		g.t.Fatal(err)
	}
	return reg
}

func (g *rig) advance(d time.Duration) {
	g.mu.Lock()
	g.now = g.now.Add(d)
	g.mu.Unlock()
}

func sortedNames(m map[string]*fakeUpstream) []string {
	var out []string
	for _, n := range []string{"a", "b", "c"} {
		if _, ok := m[n]; ok {
			out = append(out, n)
		}
	}
	return out
}

func (g *rig) opener() Opener {
	return func(ctx context.Context, up, method, path string, q url.Values, hdr http.Header) (*http.Response, error) {
		f := g.ups[up]
		req, err := http.NewRequestWithContext(ctx, method, f.srv.URL+path+"?"+q.Encode(), nil)
		if err != nil {
			return nil, err
		}
		for k, v := range hdr {
			req.Header[k] = v
		}
		return http.DefaultClient.Do(req)
	}
}

// put stores data as media source id on upstream up and returns its copy.
func (g *rig) put(up, id string, data []byte) Copy {
	g.ups[up].set(func(f *fakeUpstream) { f.files[id] = data })
	return Copy{Source: Source{Upstream: up, Item: id, Media: id}, Size: int64(len(data)), Key: "mkv|120|v:h264:640x360"}
}

func randomBytes(n int, seed uint64) []byte {
	r := rand.New(rand.NewPCG(seed, seed))
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(r.Uint32())
	}
	return b
}

func (g *rig) stream(srcs ...Copy) Stream {
	var ss []Source
	for _, c := range srcs {
		ss = append(ss, c.Source)
	}
	return Stream{Sources: ss, OneVersion: true, Open: func(ctx context.Context, src Source, method string, hdr http.Header) (*http.Response, error) {
		path, q := streamPath(src)
		return g.opener()(ctx, src.Upstream, method, path, q, hdr)
	}}
}

func (g *rig) relay(st Stream, rng string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/Videos/x/stream?static=true", nil)
	if rng != "" {
		req.Header.Set("Range", rng)
	}
	w := httptest.NewRecorder()
	g.reg.Relay(w, req, st)
	return w
}

func versions(gr Grouping) [][]string {
	var out [][]string
	for _, v := range gr.Versions {
		var names []string
		for _, c := range v {
			names = append(names, c.Upstream)
		}
		out = append(out, names)
	}
	return out
}

func TestOffsets(t *testing.T) {
	if got := Offsets(3*SampleSize + 5); len(got) != 4 || got[3] != 3*SampleSize {
		t.Errorf("small file offsets = %v (want the whole file in blocks)", got)
	}
	size := int64(61411567)
	got := Offsets(size)
	if len(got) != interior+2 || got[0] != 0 || got[len(got)-1] != size-SampleSize {
		t.Fatalf("offsets = %v", got)
	}
	for i := 1; i < len(got); i++ {
		if got[i] <= got[i-1] || (i < len(got)-1 && got[i]%4096 != 0) {
			t.Errorf("offsets not increasing and aligned: %v", got)
		}
	}
	// The rig's damaged copy zeroes 1 MiB at the middle MiB boundary; a
	// sample must land in it.
	zero := size / 2 / (1 << 20) * (1 << 20)
	hit := false
	for _, off := range got {
		hit = hit || off >= zero && off+SampleSize <= zero+(1<<20)
	}
	if !hit {
		t.Errorf("no sample in the zeroed MiB at %d: %v", zero, got)
	}
}

func TestGroupIdenticalCopies(t *testing.T) {
	g := newRig(t, "a", "b")
	data := randomBytes(3<<20, 1)
	a, b := g.put("a", "a1", data), g.put("b", "b1", data)
	gr := g.reg.Group(context.Background(), []Copy{a, b})
	if fmt.Sprint(versions(gr)) != "[[a b]]" || len(gr.Hidden) != 0 {
		t.Fatalf("versions = %v hidden %v", versions(gr), gr.Hidden)
	}
	if len(g.reg.peerSources(context.Background(), a.Source)) != 1 {
		t.Error("identical copies are not peers")
	}
	// Fingerprints are stored: a second grouping reads nothing.
	before := len(g.ups["a"].requests())
	g.reg.Group(context.Background(), []Copy{a, b})
	if len(g.ups["a"].requests()) != before {
		t.Error("fingerprints were recomputed")
	}
}

func TestGroupDamagedCopyIsHidden(t *testing.T) {
	g := newRig(t, "a", "b")
	data := randomBytes(8<<20, 2)
	bad := bytes.Clone(data)
	mid := int64(len(bad)) / 2 / (1 << 20) * (1 << 20)
	clear(bad[mid : mid+1<<20]) // like testrig/media.sh
	a, b := g.put("a", "a1", data), g.put("b", "b1", bad)

	gr := g.reg.Group(context.Background(), []Copy{a, b})
	if fmt.Sprint(versions(gr)) != "[[a]]" || len(gr.Hidden) != 1 || gr.Hidden[0].Upstream != "b" {
		t.Fatalf("versions = %v hidden %v", versions(gr), gr.Hidden)
	}
	v, _ := g.reg.verdict(b)
	if v.State != Damaged || !strings.Contains(v.Reason, "zeroed") || v.Peer.Upstream != "a" {
		t.Errorf("verdict = %+v", v)
	}
	// Persisted: a new registry knows without reading.
	g.reg = g.open()
	if v, _ := g.reg.verdict(b); v.State != Damaged {
		t.Errorf("verdict after reload = %+v", v)
	}
	// With a's copy unavailable, the damaged copy is the only one: served.
	gr = g.reg.Group(context.Background(), []Copy{b})
	if fmt.Sprint(versions(gr)) != "[[b]]" || len(gr.Hidden) != 0 {
		t.Errorf("only copy: versions = %v hidden %v", versions(gr), gr.Hidden)
	}
}

// Constant-bitrate encodes pad with 0xff filler: a good copy can be mostly
// one byte where the damaged copy is zeroed.
func TestGroupDamagedCopyOfPaddedFile(t *testing.T) {
	g := newRig(t, "a", "b")
	data := randomBytes(8<<20, 16)
	mid := int64(len(data)) / 2 / (1 << 20) * (1 << 20)
	for i := mid; i < mid+1<<20; i++ {
		if i%100 != 0 {
			data[i] = 0xff
		}
	}
	bad := bytes.Clone(data)
	clear(bad[mid : mid+1<<20])
	a, b := g.put("a", "a1", data), g.put("b", "b1", bad)
	if gr := g.reg.Group(context.Background(), []Copy{a, b}); fmt.Sprint(versions(gr)) != "[[a]]" || len(gr.Hidden) != 1 {
		t.Errorf("versions = %v hidden %v", versions(gr), gr.Hidden)
	}
	if v, _ := g.reg.verdict(a); v.State != "" {
		t.Errorf("the padded good copy got a verdict: %+v", v)
	}
}

func TestGroupTruncatedCopyIsHidden(t *testing.T) {
	g := newRig(t, "a", "b")
	data := randomBytes(5<<20, 3)
	a, b := g.put("a", "a1", data), g.put("b", "b1", data[:len(data)*6/10])
	gr := g.reg.Group(context.Background(), []Copy{a, b})
	if fmt.Sprint(versions(gr)) != "[[a]]" || len(gr.Hidden) != 1 {
		t.Fatalf("versions = %v hidden %v", versions(gr), gr.Hidden)
	}
	if v, _ := g.reg.verdict(b); v.State != Truncated || v.Peer.Upstream != "a" {
		t.Errorf("verdict = %+v", v)
	}
	// A small truncated file (fingerprinted whole) too.
	small := randomBytes(1541618, 4)
	a2, b2 := g.put("a", "a2", small), g.put("b", "b2", small[:924970])
	if gr := g.reg.Group(context.Background(), []Copy{a2, b2}); len(gr.Hidden) != 1 || gr.Hidden[0] != b2 {
		t.Errorf("small: versions = %v hidden %v", versions(gr), gr.Hidden)
	}
}

func TestGroupDifferentEncodes(t *testing.T) {
	g := newRig(t, "a", "b")
	a, b := g.put("a", "a1", randomBytes(2<<20, 5)), g.put("b", "b1", randomBytes(2<<20, 6))
	gr := g.reg.Group(context.Background(), []Copy{a, b})
	if fmt.Sprint(versions(gr)) != "[[a] [b]]" || len(gr.Hidden) != 0 {
		t.Errorf("versions = %v hidden %v", versions(gr), gr.Hidden)
	}
	for _, c := range []Copy{a, b} {
		if v, ok := g.reg.verdict(c); ok {
			t.Errorf("different encodes got a verdict: %+v", v)
		}
	}
	// A longer, unrelated file of the same encode is not "truncated".
	c := g.put("b", "b2", randomBytes(3<<20, 7))
	if gr := g.reg.Group(context.Background(), []Copy{a, c}); len(gr.Versions) != 2 || len(gr.Hidden) != 0 {
		t.Errorf("unrelated sizes: %v hidden %v", versions(gr), gr.Hidden)
	}
}

func TestGroupDisputedAndMajority(t *testing.T) {
	g := newRig(t, "a", "b", "c")
	data := randomBytes(8<<20, 8)
	flipped := bytes.Clone(data)
	off := Offsets(int64(len(data)))[5]
	copy(flipped[off:], randomBytes(4096, 9)) // bit rot that is not a fill pattern
	a, b := g.put("a", "a1", data), g.put("b", "b1", flipped)
	gr := g.reg.Group(context.Background(), []Copy{a, b})
	if fmt.Sprint(versions(gr)) != "[[a] [b]]" || len(gr.Hidden) != 0 {
		t.Fatalf("two disagreeing copies: versions = %v hidden %v", versions(gr), gr.Hidden)
	}
	for _, c := range []Copy{a, b} {
		if v, _ := g.reg.verdict(c); v.State != Disputed {
			t.Errorf("%s verdict = %+v", c.Upstream, v)
		}
	}
	// A third copy agreeing with a settles it once the daily re-check drops
	// the dispute: b is the odd one out.
	c := g.put("c", "c1", data)
	g.advance(recheckAfter + time.Minute)
	g.reg.recheckVerdicts(context.Background(), []Copy{a, b, c})
	gr = g.reg.Group(context.Background(), []Copy{a, b, c})
	if fmt.Sprint(versions(gr)) != "[[a c]]" || len(gr.Hidden) != 1 || gr.Hidden[0].Upstream != "b" {
		t.Errorf("majority: versions = %v hidden %v", versions(gr), gr.Hidden)
	}
}

func TestRank(t *testing.T) {
	g := newRig(t, "a", "b", "c")
	src := func(up string) Source { return Source{Upstream: up, Item: up + "1", Media: up + "1"} }
	order := func(ss []Source) string {
		var out []string
		for _, s := range ss {
			out = append(out, s.Upstream)
		}
		return strings.Join(out, "")
	}
	all := []Source{src("c"), src("b"), src("a")}
	if got := order(g.reg.Rank(all)); got != "abc" {
		t.Errorf("unmeasured: %s, want config order", got)
	}
	// a is measured slow; b and c are not measured yet (optimistic).
	g.reg.observeThroughput("a", 1<<20, time.Second)
	if got := order(g.reg.Rank(all)); got != "bca" {
		t.Errorf("a slow: %s", got)
	}
	// Near-equal speeds keep config order; clearly faster wins.
	g.reg.observeThroughput("b", 100<<20, time.Second)
	g.reg.observeThroughput("c", 110<<20, time.Second)
	if got := order(g.reg.Rank(all)); got != "bca" {
		t.Errorf("b and c alike: %s", got)
	}
	g.reg.observeThroughput("c", 1000<<20, time.Second) // EWMA moves toward it
	g.reg.observeThroughput("c", 1000<<20, time.Second)
	if got := order(g.reg.Rank(all)); got != "cba" {
		t.Errorf("c much faster: %s", got)
	}
	// Health first: a failure, a down upstream, a quarantine, a broken copy.
	// A recent failure outranks speed: stability first.
	g.reg.Failed(src("c"), "test")
	if got := order(g.reg.Rank(all)); got != "bac" {
		t.Errorf("c failed once: %s", got)
	}
	g.reg.upstreamFailed("b", fmt.Errorf("refused"))
	if got := order(g.reg.Rank(all)); got != "acb" {
		t.Errorf("b down: %s", got)
	}
	// Failures are forgiven after a while.
	g.advance(failureWindow + time.Second)
	if got := order(g.reg.Rank(all)); got != "cab" {
		t.Errorf("c's failure is old: %s", got)
	}
	g.reg.Failed(src("c"), "test")
	g.reg.Failed(src("c"), "test")
	if g.reg.Quarantined(src("c")) {
		t.Fatal("failures counted across the forgiveness window")
	}
	g.reg.Failed(src("c"), "test")
	if !g.reg.Quarantined(src("c")) {
		t.Fatal("three failures do not quarantine")
	}
	if got := order(g.reg.Rank(all)); got != "abc" {
		t.Errorf("c quarantined, b down: %s", got)
	}
	g.reg.setVerdict(context.Background(), Copy{Source: src("a")}, Verdict{State: Damaged})
	if got := order(g.reg.Rank(all)); got != "bca" {
		t.Errorf("a broken: %s", got)
	}
}

func TestQuarantineBackoffAndRecheck(t *testing.T) {
	g := newRig(t, "a")
	data := randomBytes(1<<20, 10)
	a := g.put("a", "a1", data)
	g.ups["a"].set(func(f *fakeUpstream) { f.status = 500 })
	for range 3 {
		g.relay(g.stream(a), "")
	}
	if !g.reg.Quarantined(a.Source) {
		t.Fatal("not quarantined after three failed relays")
	}
	// Persisted.
	g.reg = g.open()
	if !g.reg.Quarantined(a.Source) {
		t.Fatal("quarantine lost on reload")
	}
	// Backoff ends; the re-check fails; the next backoff is longer.
	g.advance(minBackoff + time.Second)
	g.reg.recheckQuarantined(context.Background())
	if !g.reg.Quarantined(a.Source) {
		t.Fatal("failed re-check ended the quarantine")
	}
	g.advance(minBackoff + time.Second)
	if !g.reg.Quarantined(a.Source) {
		t.Error("backoff did not grow")
	}
	// The file is fixed: the next re-check ends the quarantine.
	g.ups["a"].set(func(f *fakeUpstream) { f.status = 0 })
	g.advance(2 * minBackoff)
	g.reg.recheckQuarantined(context.Background())
	if g.reg.Quarantined(a.Source) {
		t.Error("successful re-check did not end the quarantine")
	}
	if n := countRows(t, g.db, "source_health"); n != 0 {
		t.Errorf("%d health rows remain", n)
	}
}

func countRows(t *testing.T, db *sql.DB, table string) int {
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// identical stores a file on a and b and fingerprints both.
func identical(t *testing.T, g *rig, size int) ([]byte, Copy, Copy) {
	data := randomBytes(size, uint64(size))
	a, b := g.put("a", "a1", data), g.put("b", "b1", data)
	if gr := g.reg.Group(context.Background(), []Copy{a, b}); len(gr.Versions) != 1 {
		t.Fatalf("copies not identical: %v", versions(gr))
	}
	return data, a, b
}

func TestRelayFailsOverMidBody(t *testing.T) {
	for _, tc := range []struct {
		name  string
		fault func(f *fakeUpstream)
		rng   string
	}{
		{"cut", func(f *fakeUpstream) { f.cutAfter = 1500 << 10 }, ""},
		{"cut, range", func(f *fakeUpstream) { f.cutAfter = 1 << 20 }, "bytes=1000000-"},
		{"stall", func(f *fakeUpstream) { f.stallAfter = 1 << 20 }, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newRig(t, "a", "b")
			data, a, b := identical(t, g, 5<<20)
			g.ups["a"].set(tc.fault)
			w := g.relay(g.stream(a, b), tc.rng)
			want, code := data, http.StatusOK
			if tc.rng != "" {
				want, code = data[1000000:], http.StatusPartialContent
				if cr := w.Header().Get("Content-Range"); cr != fmt.Sprintf("bytes 1000000-%d/%d", len(data)-1, len(data)) {
					t.Errorf("Content-Range = %q", cr)
				}
			}
			if w.Code != code || !bytes.Equal(w.Body.Bytes(), want) {
				t.Fatalf("relay = %d, %d bytes (want %d, identical: %v)", w.Code, w.Body.Len(), len(want), bytes.Equal(w.Body.Bytes(), want))
			}
			if w.Header().Get("Last-Modified") != "" {
				t.Error("per-upstream validators leak to the client")
			}
			// b was asked for exactly the rest.
			got := g.ups["b"].requests()
			last := got[len(got)-1]
			if !strings.HasPrefix(last, "bytes=") || strings.HasPrefix(last, "bytes=0-") || !strings.HasSuffix(last, fmt.Sprintf("-%d", len(data)-1)) {
				t.Errorf("b got Range %q", last)
			}
		})
	}
}

func TestRelaySkipsFailingSources(t *testing.T) {
	g := newRig(t, "a", "b")
	data, a, b := identical(t, g, 1<<20)
	g.ups["a"].set(func(f *fakeUpstream) { f.status = 503 })
	if w := g.relay(g.stream(a, b), ""); !bytes.Equal(w.Body.Bytes(), data) {
		t.Errorf("5xx on a: got %d bytes", w.Body.Len())
	}
	// a fails at connecting: marked down, skipped, ranked after b.
	g.reg.Succeeded(a.Source)
	g.ups["a"].srv.Close()
	if w := g.relay(g.stream(a, b), "bytes=0-99"); w.Code != http.StatusPartialContent || !bytes.Equal(w.Body.Bytes(), data[:100]) {
		t.Errorf("a down: %d %d bytes", w.Code, w.Body.Len())
	}
	if !g.reg.Down("a") {
		t.Error("connection failure did not mark a down")
	}
	if got := g.reg.Rank([]Source{a.Source, b.Source}); got[0].Upstream != "b" {
		t.Errorf("rank after a went down: %v", got)
	}
}

func TestRelayNeverSplicesDifferentFiles(t *testing.T) {
	g := newRig(t, "a", "b")
	a, b := g.put("a", "a1", randomBytes(3<<20, 11)), g.put("b", "b1", randomBytes(3<<20, 12))
	g.reg.Group(context.Background(), []Copy{a, b})
	g.ups["a"].set(func(f *fakeUpstream) { f.cutAfter = 1 << 20 })
	w := g.relay(g.stream(a, b), "")
	if w.Body.Len() != 1<<20 {
		t.Errorf("relayed %d bytes", w.Body.Len())
	}
	for _, rng := range g.ups["b"].requests() {
		if rng != "" && !strings.HasPrefix(rng, "bytes=") {
			continue
		}
		if strings.HasPrefix(rng, fmt.Sprintf("bytes=%d-", 1<<20)) {
			t.Error("continued a's bytes with a different file from b")
		}
	}
}

func TestRelayFindsDamageDuringPlayback(t *testing.T) {
	g := newRig(t, "a", "b")
	data := randomBytes(40<<20, 13)
	bad := bytes.Clone(data)
	// Damage a block no sample covers, so the copies fingerprint alike.
	block := int64(-1)
	for blk := int64(1); block < 0; blk++ {
		covered := false
		for _, off := range Offsets(int64(len(data))) {
			covered = covered || off+SampleSize > blk<<20 && off < (blk+1)<<20
		}
		if !covered {
			block = blk
		}
	}
	clear(bad[block<<20 : (block+1)<<20])
	a, b := g.put("a", "a1", data), g.put("b", "b1", bad)
	if gr := g.reg.Group(context.Background(), []Copy{a, b}); len(gr.Versions) != 1 {
		t.Fatalf("versions = %v", versions(gr))
	}
	// Play a's copy once: its block hashes are recorded.
	if w := g.relay(g.stream(a), ""); !bytes.Equal(w.Body.Bytes(), data) {
		t.Fatal("first relay")
	}
	// Then b's: the disagreement on that block is settled; b is damaged.
	g.relay(g.stream(b), "")
	deadline := time.Now().Add(5 * time.Second)
	for !g.reg.broken(b) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if v, _ := g.reg.verdict(b); v.State != Damaged || v.Offset != block<<20 {
		t.Fatalf("verdict = %+v", v)
	}
	// From now on plays avoid b, and grouping hides it.
	if w := g.relay(g.stream(b, a), ""); !bytes.Equal(w.Body.Bytes(), data) {
		t.Error("relay after the verdict served damaged bytes")
	}
	if gr := g.reg.Group(context.Background(), []Copy{a, b}); len(gr.Hidden) != 1 {
		t.Errorf("grouping after the verdict: %v hidden %v", versions(gr), gr.Hidden)
	}
}

func TestRelaySwitchesWhenCopyFoundDamaged(t *testing.T) {
	g := newRig(t, "a", "b")
	data, a, b := identical(t, g, 40<<20)
	// Mark b broken while it is being relayed: the relay moves to a.
	g.ups["b"].set(func(f *fakeUpstream) { f.slow = 2 * time.Millisecond })
	st := g.stream(b, a)
	st.Sources = []Source{b.Source, a.Source}
	g.reg.observeThroughput("a", 1, time.Second) // rank b first
	go func() {
		time.Sleep(50 * time.Millisecond)
		g.reg.setVerdict(context.Background(), b, Verdict{State: Damaged, Reason: "test"})
	}()
	w := g.relay(st, "")
	if !bytes.Equal(w.Body.Bytes(), data) {
		t.Fatalf("got %d bytes, identical %v", w.Body.Len(), bytes.Equal(w.Body.Bytes(), data))
	}
	if len(g.ups["a"].requests()) == 0 {
		t.Error("a was never asked")
	}
}

func TestSessions(t *testing.T) {
	g := newRig(t, "a", "b")
	_, a, b := identical(t, g, 1<<20)
	g.reg.StartSession(Session{ID: "ps1", User: "u", Device: "d", Item: "item", Versions: map[string][]Source{"v": {a.Source, b.Source}}})
	if got := g.reg.Prefer("u", "d", []string{"ps1"}); got != "" {
		t.Errorf("before any stream: %q", got)
	}
	g.ups["a"].set(func(f *fakeUpstream) { f.status = 500 })
	st := g.stream(a, b)
	st.Session = "ps1"
	g.relay(st, "")
	if got := g.reg.Prefer("u", "d", []string{"ps1"}); got != "b" {
		t.Errorf("after failing over: %q", got)
	}
	// Found by item for the device when the request has no play session.
	if got := g.reg.Prefer("u", "d", []string{"item"}); got != "b" {
		t.Errorf("by item: %q", got)
	}
	if got := g.reg.Prefer("u", "other", []string{"item"}); got != "" {
		t.Errorf("other device: %q", got)
	}
	// a recovers and is preferred again by rank, but the session stays on b.
	g.ups["a"].set(func(f *fakeUpstream) { f.status = 0 })
	g.reg.Succeeded(a.Source)
	g.relay(st, "")
	if got := g.reg.Prefer("u", "d", []string{"ps1"}); got != "b" {
		t.Errorf("session moved back without a failure: %q", got)
	}
	g.reg.Moved("u", "d", []string{"ps1"}, "b", "a")
	if got := g.reg.Prefer("u", "d", []string{"ps1"}); got != "a" {
		t.Errorf("after Moved: %q", got)
	}
	g.advance(sessionIdle + time.Minute)
	g.reg.expireSessions()
	if got := g.reg.Prefer("u", "d", []string{"ps1"}); got != "" {
		t.Errorf("idle session kept: %q", got)
	}
}

func TestReport(t *testing.T) {
	g := newRig(t, "a", "b")
	data := randomBytes(2<<20, 14)
	a, b := g.put("a", "a1", data), g.put("b", "b1", data[:1<<20])
	single := g.put("a", "a2", randomBytes(1<<20, 15))
	dto := func(c Copy, name string) []byte {
		return fmt.Appendf(nil, `{"Name":%q,"ProductionYear":2006,"MediaSources":[{"Id":%q,"Size":%d,"Container":"mkv","RunTimeTicks":1200000000,"Path":"/media/%s.mkv"}]}`,
			name, c.Media, c.Size, name)
	}
	g.reg.opts.Library = LibraryFunc(func(ctx context.Context, fn func(LibraryItem) error) error {
		if err := fn(LibraryItem{ID: "u1", Type: "Movie", Copies: []LibraryCopy{
			{Upstream: "a", Item: "a1", DTO: dto(a, "Truncated")}, {Upstream: "b", Item: "b1", DTO: dto(b, "Truncated")}}}); err != nil {
			return err
		}
		return fn(LibraryItem{ID: "u2", Type: "Movie", Copies: []LibraryCopy{{Upstream: "a", Item: "a2", DTO: dto(single, "Lonely")}}})
	})
	g.reg.Verify(context.Background())
	rep, err := g.reg.Report(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Totals.Items != 2 || rep.Totals.MultiCopy != 1 || rep.Totals.Unchecked != 0 || rep.Totals.Broken != 1 || rep.Totals.SingleCopy != 2 {
		t.Errorf("totals = %+v", rep.Totals)
	}
	if len(rep.Broken) != 1 || rep.Broken[0].Name != "Truncated (2006)" || rep.Broken[0].Copies[1].State != Truncated ||
		rep.Broken[0].Copies[1].Path != "/media/Truncated.mkv" {
		t.Errorf("broken = %+v", rep.Broken)
	}
	if len(rep.Upstreams) != 2 {
		t.Errorf("upstreams = %+v", rep.Upstreams)
	}
	// Copies that no longer exist are forgotten.
	g.reg.opts.Library = LibraryFunc(func(ctx context.Context, fn func(LibraryItem) error) error { return nil })
	g.reg.Verify(context.Background())
	for _, table := range []string{"source_fingerprints", "source_verdicts"} {
		if n := countRows(t, g.db, table); n != 0 {
			t.Errorf("%s keeps %d rows for gone copies", table, n)
		}
	}
}

func TestEncodeKey(t *testing.T) {
	ms := func(js string) map[string]any {
		var m map[string]any
		dec := jsonDecoder(js)
		if err := dec.Decode(&m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	a := ms(`{"Container":"mkv","Size":100,"RunTimeTicks":72000000000,"MediaStreams":[{"Type":"Video","Codec":"h264","Width":1920,"Height":1080},{"Type":"Audio","Codec":"aac","Channels":2}]}`)
	trunc := ms(`{"Container":"MKV","Size":60,"RunTimeTicks":72000400000,"Bitrate":5,"MediaStreams":[{"Type":"Video","Codec":"H264","Width":1920,"Height":1080},{"Type":"Audio","Codec":"AAC","Channels":2}]}`)
	other := ms(`{"Container":"mkv","Size":100,"RunTimeTicks":72000000000,"MediaStreams":[{"Type":"Video","Codec":"hevc","Width":1920,"Height":1080}]}`)
	if EncodeKey(a) != EncodeKey(trunc) {
		t.Errorf("a truncated copy keeps its encode key:\n%s\n%s", EncodeKey(a), EncodeKey(trunc))
	}
	if EncodeKey(a) == EncodeKey(other) {
		t.Error("different codecs share a key")
	}
}

func jsonDecoder(js string) *json.Decoder {
	dec := json.NewDecoder(strings.NewReader(js))
	dec.UseNumber()
	return dec
}
