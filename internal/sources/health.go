package sources

import (
	"cmp"
	"context"
	"errors"
	"math"
	"slices"
	"time"
)

const (
	quarantineAfter = 3 // failures, each within failureWindow of the last
	failureWindow   = 10 * time.Minute
	minBackoff      = time.Minute
	maxBackoff      = time.Hour
	// speedChunk is the request size speeds are compared on: latency
	// dominates small reads, throughput large ones.
	speedChunk = 4 << 20
	// speedMargin is how much faster an upstream must be to beat one that
	// comes earlier in the config, so near-equal upstreams don't flap.
	speedMargin = 1.25
)

// ewma is an exponentially weighted moving average.
type ewma struct {
	v float64
	n int
}

func (e *ewma) add(x float64) {
	if e.n == 0 {
		e.v = x
	} else {
		e.v = 0.7*e.v + 0.3*x
	}
	e.n++
}

// upstreamStats are measured from real relays and pings, kept in memory.
type upstreamStats struct {
	latency    ewma // seconds
	throughput ewma // bytes per second
	down       bool
	downErr    string
	since      time.Time // of the current up/down state
	pingFails  int
}

// health tracks one source's recent failures and quarantine.
type health struct {
	item     string
	failures int
	last     time.Time // of the last failure
	reason   string
	until    time.Time     // quarantined until
	backoff  time.Duration // next quarantine length
}

func (r *Registry) statsOf(up string) *upstreamStats {
	s := r.stats[up]
	if s == nil {
		s = &upstreamStats{}
		r.stats[up] = s
	}
	return s
}

func (r *Registry) observeLatency(up string, d time.Duration) {
	r.mu.Lock()
	r.statsOf(up).latency.add(d.Seconds())
	r.mu.Unlock()
}

func (r *Registry) observeThroughput(up string, bytes int64, d time.Duration) {
	if d <= 0 || bytes <= 0 {
		return
	}
	r.mu.Lock()
	r.statsOf(up).throughput.add(float64(bytes) / d.Seconds())
	r.mu.Unlock()
}

// upstreamFailed marks an upstream unreachable after a connection error.
// Context cancellation is the client leaving, not a failure.
func (r *Registry) upstreamFailed(up string, err error) {
	if errors.Is(err, context.Canceled) {
		return
	}
	r.mu.Lock()
	s := r.statsOf(up)
	if !s.down {
		s.down, s.since = true, r.now()
		r.log.Warn("upstream unreachable", "upstream", up, "err", err)
	}
	s.downErr = err.Error()
	r.mu.Unlock()
}

// UpstreamFailed records a connection failure seen by a request.
func (r *Registry) UpstreamFailed(up string, err error) { r.upstreamFailed(up, err) }

// UpstreamOK records that a request reached an upstream.
func (r *Registry) UpstreamOK(up string) { r.upstreamOK(up) }

func (r *Registry) upstreamOK(up string) {
	r.mu.Lock()
	s := r.statsOf(up)
	if s.down {
		s.down, s.since = false, r.now()
		r.log.Info("upstream reachable again", "upstream", up)
	}
	s.pingFails = 0
	r.mu.Unlock()
}

// Down reports whether an upstream is currently unreachable.
func (r *Registry) Down(up string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.statsOf(up).down
}

// Failed records that a source failed to serve its file (an error status,
// a short body, or a stall). Repeated failures quarantine it.
func (r *Registry) Failed(src Source, reason string) {
	r.mu.Lock()
	h := r.health[src.key()]
	if h == nil {
		h = &health{}
		r.health[src.key()] = h
	}
	if r.now().Sub(h.last) > failureWindow && !h.until.After(r.now()) && h.until.IsZero() {
		h.failures = 0 // old failures are forgiven
	}
	h.item = src.Item
	h.failures++
	h.last = r.now()
	h.reason = reason
	quarantine := h.failures >= quarantineAfter && !h.until.After(r.now())
	if quarantine {
		h.backoff = min(max(h.backoff*2, minBackoff), maxBackoff)
		h.until = r.now().Add(h.backoff)
	}
	until, backoff := h.until, h.backoff
	r.mu.Unlock()
	r.log.Warn("source failed", "source", src.String(), "reason", reason)
	if quarantine {
		r.log.Warn("source quarantined", "source", src.String(), "until", until.Format(time.RFC3339), "reason", reason)
		r.saveHealth(src, reason, until, backoff)
	}
}

// Succeeded records that a source served its file; it ends any quarantine.
func (r *Registry) Succeeded(src Source) {
	r.mu.Lock()
	h := r.health[src.key()]
	delete(r.health, src.key())
	r.mu.Unlock()
	if h != nil && !h.until.IsZero() {
		r.log.Info("source healthy again", "source", src.String())
		if _, err := r.db.Exec(`DELETE FROM source_health WHERE upstream = ? AND media = ?`, src.Upstream, src.Media); err != nil {
			r.log.Error("saving source health", "err", err)
		}
	}
}

func (r *Registry) saveHealth(src Source, reason string, until time.Time, backoff time.Duration) {
	_, err := r.db.Exec(`INSERT INTO source_health (upstream, media, item, reason, until, backoff) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (upstream, media) DO UPDATE SET item = excluded.item, reason = excluded.reason, until = excluded.until, backoff = excluded.backoff`,
		src.Upstream, src.Media, src.Item, reason, until.UnixMilli(), backoff.Milliseconds())
	if err != nil {
		r.log.Error("saving source health", "err", err)
	}
}

// Quarantined reports whether a source is quarantined now.
func (r *Registry) Quarantined(src Source) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	h := r.health[src.key()]
	return h != nil && h.until.After(r.now())
}

// tier orders sources by health: lower is better.
func (r *Registry) tier(s Source) int {
	h := r.health[s.key()]
	switch v, ok := r.verdicts[s.key()]; {
	case ok && v.Broken():
		return 4
	case h != nil && h.until.After(r.now()):
		return 3
	case r.statsOf(s.Upstream).down:
		return 2
	case h != nil && h.failures > 0 && r.now().Sub(h.last) < failureWindow:
		return 1
	}
	return 0
}

// estimate is the expected seconds to fetch speedChunk from an upstream, or
// 0 when it is unmeasured.
func (r *Registry) estimate(up string) float64 {
	s := r.statsOf(up)
	if s.throughput.n == 0 {
		return 0
	}
	return s.latency.v + speedChunk/s.throughput.v
}

// Rank orders sources best first: by health; then speed, where an upstream
// must be clearly faster to pass one earlier in the config; then config
// order. Sources keep their relative order otherwise.
func (r *Registry) Rank(srcs []Source) []Source {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := slices.Clone(srcs)
	tiers := map[key]int{}
	est := map[string]float64{}
	for _, s := range out {
		tiers[s.key()] = r.tier(s)
		est[s.Upstream] = r.estimate(s.Upstream)
	}
	// Speed groups: everything within speedMargin of the fastest is group 0
	// (ordered by config); slower upstreams follow, fastest first. An
	// unmeasured upstream counts as fastest, so it gets tried and measured
	// once: one play each, then plays go where they are fastest.
	best := math.Inf(1)
	for _, e := range est {
		best = min(best, e)
	}
	group := func(up string) float64 {
		if e := est[up]; e > 0 && e > best*speedMargin {
			return e
		}
		return 0
	}
	slices.SortStableFunc(out, func(a, b Source) int {
		return cmp.Or(
			cmp.Compare(tiers[a.key()], tiers[b.key()]),
			cmp.Compare(group(a.Upstream), group(b.Upstream)),
			cmp.Compare(r.configOrder(a.Upstream), r.configOrder(b.Upstream)),
		)
	})
	return out
}

func (r *Registry) configOrder(up string) int {
	if i, ok := r.order[up]; ok {
		return i
	}
	return len(r.order)
}
