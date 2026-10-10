// Package metrics keeps counters and histograms in memory and serves them,
// with gauges read at scrape time, in the Prometheus text format. It is a
// small dependency-free subset: each metric has fixed label names.
//
// Metrics are served on their own address (metrics_listen), never on the
// client-facing port, which must look like Jellyfin's.
package metrics

import (
	"fmt"
	"io"
	"math"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Registry holds metrics. Default is the one the packages register with.
type Registry struct {
	mu      sync.Mutex
	metrics []metric
	gauges  []*GaugeFunc
}

// Default is the process-wide registry.
var Default = &Registry{}

type metric interface {
	write(w io.Writer)
}

// series is one label combination's value(s).
type series struct {
	labels []string
	value  float64
	counts []uint64 // histograms: per bucket
	sum    float64
	count  uint64
}

type family struct {
	name, help, kind string
	labels           []string
	mu               sync.Mutex
	series           map[string]*series
}

func (f *family) get(values []string) *series {
	if len(values) != len(f.labels) {
		panic(fmt.Sprintf("metrics: %s takes %d label values, got %d", f.name, len(f.labels), len(values)))
	}
	k := strings.Join(values, "\x00")
	s := f.series[k]
	if s == nil {
		s = &series{labels: slices.Clone(values)}
		f.series[k] = s
	}
	return s
}

func (f *family) header(w io.Writer) {
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", f.name, f.help, f.name, f.kind)
}

func (f *family) sorted() []*series {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*series, 0, len(f.series))
	for _, s := range f.series {
		c := *s
		c.counts = slices.Clone(s.counts)
		out = append(out, &c)
	}
	sort.Slice(out, func(i, j int) bool { return strings.Join(out[i].labels, "\x00") < strings.Join(out[j].labels, "\x00") })
	return out
}

func labelString(names, values []string, extra ...string) string {
	if len(names) == 0 && len(extra) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteByte('{')
	for i, n := range names {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(n + `="` + escape(values[i]) + `"`)
	}
	for i := 0; i+1 < len(extra); i += 2 {
		if b.Len() > 1 {
			b.WriteByte(',')
		}
		b.WriteString(extra[i] + `="` + escape(extra[i+1]) + `"`)
	}
	b.WriteByte('}')
	return b.String()
}

func escape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(s)
}

func number(v float64) string {
	switch {
	case math.IsInf(v, 1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

func (r *Registry) add(m metric) {
	r.mu.Lock()
	r.metrics = append(r.metrics, m)
	r.mu.Unlock()
}

// Counter is a monotonically increasing count per label combination.
type Counter struct{ f *family }

// NewCounter registers a counter.
func (r *Registry) NewCounter(name, help string, labels ...string) *Counter {
	c := &Counter{&family{name: name, help: help, kind: "counter", labels: labels, series: map[string]*series{}}}
	r.add(c)
	return c
}

// Add adds v (>= 0) for the label values.
func (c *Counter) Add(v float64, labels ...string) {
	c.f.mu.Lock()
	c.f.get(labels).value += v
	c.f.mu.Unlock()
}

// Inc adds 1.
func (c *Counter) Inc(labels ...string) { c.Add(1, labels...) }

func (c *Counter) write(w io.Writer) {
	c.f.header(w)
	for _, s := range c.f.sorted() {
		fmt.Fprintf(w, "%s%s %s\n", c.f.name, labelString(c.f.labels, s.labels), number(s.value))
	}
}

// Histogram counts observations into buckets.
type Histogram struct {
	f       *family
	buckets []float64
}

// NewHistogram registers a histogram with the given upper bounds.
func (r *Registry) NewHistogram(name, help string, buckets []float64, labels ...string) *Histogram {
	h := &Histogram{&family{name: name, help: help, kind: "histogram", labels: labels, series: map[string]*series{}}, slices.Clone(buckets)}
	slices.Sort(h.buckets)
	r.add(h)
	return h
}

// Observe records v for the label values.
func (h *Histogram) Observe(v float64, labels ...string) {
	h.f.mu.Lock()
	s := h.f.get(labels)
	if s.counts == nil {
		s.counts = make([]uint64, len(h.buckets))
	}
	for i, b := range h.buckets {
		if v <= b {
			s.counts[i]++
		}
	}
	s.sum += v
	s.count++
	h.f.mu.Unlock()
}

func (h *Histogram) write(w io.Writer) {
	h.f.header(w)
	for _, s := range h.f.sorted() {
		for i, b := range h.buckets {
			var n uint64
			if s.counts != nil {
				n = s.counts[i]
			}
			fmt.Fprintf(w, "%s_bucket%s %d\n", h.f.name, labelString(h.f.labels, s.labels, "le", number(b)), n)
		}
		fmt.Fprintf(w, "%s_bucket%s %d\n", h.f.name, labelString(h.f.labels, s.labels, "le", "+Inf"), s.count)
		fmt.Fprintf(w, "%s_sum%s %s\n", h.f.name, labelString(h.f.labels, s.labels), number(s.sum))
		fmt.Fprintf(w, "%s_count%s %d\n", h.f.name, labelString(h.f.labels, s.labels), s.count)
	}
}

// GaugeFunc reads its values when scraped.
type GaugeFunc struct {
	name, help string
	labels     []string
	read       func(emit func(v float64, labels ...string))
}

// NewGaugeFunc registers a gauge whose values read calls emit for, once per
// label combination, at each scrape.
func (r *Registry) NewGaugeFunc(name, help string, labels []string, read func(emit func(v float64, labels ...string))) {
	g := &GaugeFunc{name, help, labels, read}
	r.mu.Lock()
	r.gauges = append(r.gauges, g)
	r.mu.Unlock()
}

func (g *GaugeFunc) write(w io.Writer) {
	type row struct {
		labels []string
		v      float64
	}
	var rows []row
	g.read(func(v float64, labels ...string) { rows = append(rows, row{slices.Clone(labels), v}) })
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s gauge\n", g.name, g.help, g.name)
	sort.Slice(rows, func(i, j int) bool {
		return strings.Join(rows[i].labels, "\x00") < strings.Join(rows[j].labels, "\x00")
	})
	for _, r := range rows {
		fmt.Fprintf(w, "%s%s %s\n", g.name, labelString(g.labels, r.labels), number(r.v))
	}
}

// Write writes every metric in the Prometheus text format.
func (r *Registry) Write(w io.Writer) {
	r.mu.Lock()
	ms := slices.Clone(r.metrics)
	gs := slices.Clone(r.gauges)
	r.mu.Unlock()
	for _, m := range ms {
		m.write(w)
	}
	for _, g := range gs {
		g.write(w)
	}
}

// ServeHTTP serves the metrics.
func (r *Registry) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	r.Write(w)
}

// DurationBuckets suit request latencies, in seconds.
var DurationBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30}
