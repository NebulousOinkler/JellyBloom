package sources

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Report is the redundancy report: what needs backing up or replacing.
// JellyBloom never copies files itself.
type Report struct {
	Generated time.Time
	// Upstreams is live state; empty when produced outside the server.
	Upstreams []UpstreamReport `json:",omitempty"`
	Totals    ReportTotals
	// SingleCopy lists items with a good copy on at most one upstream: one
	// failure away from unavailable.
	SingleCopy []ReportItem
	// Broken lists items with a truncated or damaged copy.
	Broken []ReportItem
	// Disputed lists items whose copies differ with no way to tell which is
	// intact; they are offered as separate versions.
	Disputed []ReportItem
	// Quarantined lists items with a copy that keeps failing.
	Quarantined []ReportItem
}

// ReportTotals counts items.
type ReportTotals struct {
	Items       int // playable items
	MultiCopy   int // with copies on more than one upstream
	Unchecked   int // multi-copy items whose copies are not all fingerprinted yet
	SingleCopy  int
	Broken      int
	Disputed    int
	Quarantined int
}

// UpstreamReport is an upstream's measured state.
type UpstreamReport struct {
	Name           string
	Reachable      bool
	Since          *time.Time `json:",omitempty"` // of the current state
	Error          string     `json:",omitempty"`
	LatencyMs      float64    `json:",omitempty"`
	ThroughputMBps float64    `json:",omitempty"`
}

// ReportItem is an item and all of its copies.
type ReportItem struct {
	Id     string // JellyBloom item ID
	Name   string
	Type   string
	Copies []ReportCopy
}

// ReportCopy is one copy, as its upstream sees it.
type ReportCopy struct {
	Upstream         string
	ItemId           string
	MediaSourceId    string
	Path             string `json:",omitempty"`
	Size             int64
	State            string     // ok, truncated, damaged, disputed, quarantined
	Reason           string     `json:",omitempty"`
	QuarantinedUntil *time.Time `json:",omitempty"`
}

// Report builds the redundancy report from the library and stored checks.
func (r *Registry) Report(ctx context.Context) (*Report, error) {
	rep := &Report{Generated: r.now().UTC()}
	if r.opts.Open != nil {
		r.mu.Lock()
		for _, name := range r.opts.Upstreams {
			s := r.statsOf(name)
			u := UpstreamReport{Name: name, Reachable: !s.down}
			if !s.since.IsZero() {
				t := s.since.UTC()
				u.Since = &t
			}
			if s.down {
				u.Error = s.downErr
			}
			if s.latency.n > 0 {
				u.LatencyMs = round(s.latency.v * 1000)
			}
			if s.throughput.n > 0 {
				u.ThroughputMBps = round(s.throughput.v / 1e6)
			}
			rep.Upstreams = append(rep.Upstreams, u)
		}
		r.mu.Unlock()
	}
	if r.opts.Library == nil {
		return rep, nil
	}
	err := r.opts.Library.EachItem(ctx, func(it LibraryItem) error {
		copies := Copies(it)
		if len(copies) == 0 {
			return nil
		}
		rep.Totals.Items++
		paths := pathsOf(it)
		item := ReportItem{Id: it.ID, Type: it.Type, Name: displayName(it)}
		good := map[string]bool{}
		ups := map[string]bool{}
		var broken, disputed, quarantined bool
		for _, c := range copies {
			ups[c.Upstream] = true
			rc := ReportCopy{Upstream: c.Upstream, ItemId: c.Item, MediaSourceId: c.Media, Path: paths[c.key()], Size: c.Size, State: "ok"}
			if v, ok := r.verdict(c); ok {
				rc.State, rc.Reason = v.State, v.Reason
				broken = broken || v.Broken()
				disputed = disputed || v.State == Disputed
			}
			r.mu.Lock()
			if h := r.health[c.key()]; h != nil && h.until.After(r.now()) {
				until := h.until.UTC()
				rc.QuarantinedUntil = &until
				if rc.State == "ok" {
					rc.State, rc.Reason = "quarantined", h.reason
				}
				quarantined = true
			}
			r.mu.Unlock()
			if rc.State != Truncated && rc.State != Damaged {
				good[c.Upstream] = true
			}
			item.Copies = append(item.Copies, rc)
		}
		if len(ups) > 1 {
			rep.Totals.MultiCopy++
			if r.unchecked(ctx, copies) {
				rep.Totals.Unchecked++
			}
		}
		add := func(list *[]ReportItem, n *int, on bool) {
			if on {
				*list = append(*list, item)
				*n++
			}
		}
		add(&rep.SingleCopy, &rep.Totals.SingleCopy, len(good) <= 1)
		add(&rep.Broken, &rep.Totals.Broken, broken)
		add(&rep.Disputed, &rep.Totals.Disputed, disputed)
		add(&rep.Quarantined, &rep.Totals.Quarantined, quarantined)
		return nil
	})
	if err != nil {
		return nil, err
	}
	for _, list := range [][]ReportItem{rep.SingleCopy, rep.Broken, rep.Disputed, rep.Quarantined} {
		slices.SortFunc(list, func(a, b ReportItem) int {
			if a.Name != b.Name {
				if a.Name < b.Name {
					return -1
				}
				return 1
			}
			return 0
		})
	}
	return rep, nil
}

func round(f float64) float64 { return float64(int64(f*10+0.5)) / 10 }

// unchecked reports whether copies that grouping compares lack fingerprints.
func (r *Registry) unchecked(ctx context.Context, copies []Copy) bool {
	fps, err := r.fingerprints(ctx, copies)
	if err != nil {
		return true
	}
	sizes := map[string]int{}
	parts := partition(copies)
	for _, p := range parts {
		sizes[p[0].Key]++
	}
	for _, p := range parts {
		if p[0].Size <= 0 || (len(p) < 2 && sizes[p[0].Key] < 2) {
			continue
		}
		for _, c := range p {
			if _, ok := fps[c.key()]; !ok && !r.broken(c) {
				return true
			}
		}
	}
	return false
}

// pathsOf maps each copy to its file path on its upstream.
func pathsOf(it LibraryItem) map[key]string {
	out := map[key]string{}
	for _, lc := range it.Copies {
		var dto struct {
			MediaSources []struct{ Id, Path string }
		}
		if json.NewDecoder(bytes.NewReader(lc.DTO)).Decode(&dto) != nil {
			continue
		}
		for _, ms := range dto.MediaSources {
			id := normID(ms.Id)
			if id == "" {
				id = normID(lc.Item)
			}
			out[key{lc.Upstream, id}] = ms.Path
		}
	}
	return out
}

// displayName names an item the way a person looks for it.
func displayName(it LibraryItem) string {
	if len(it.Copies) == 0 {
		return ""
	}
	var d struct {
		Name, SeriesName, Album string
		AlbumArtist             string
		ParentIndexNumber       *int
		IndexNumber             *int
		ProductionYear          int
	}
	json.Unmarshal(it.Copies[0].DTO, &d)
	switch {
	case it.Type == "Episode" && d.SeriesName != "":
		s := d.SeriesName
		if d.ParentIndexNumber != nil && d.IndexNumber != nil {
			s += fmt.Sprintf(" S%02dE%02d", *d.ParentIndexNumber, *d.IndexNumber)
		}
		if strings.HasPrefix(d.Name, s) { // unnamed episodes are named after the file
			return d.Name
		}
		return s + " " + d.Name
	case d.Album != "":
		if d.AlbumArtist != "" {
			return d.AlbumArtist + " – " + d.Album + " – " + d.Name
		}
		return d.Album + " – " + d.Name
	case d.ProductionYear > 0 && (it.Type == "Movie" || it.Type == "Video"):
		if year := fmt.Sprintf("(%d)", d.ProductionYear); !strings.HasSuffix(d.Name, year) {
			return d.Name + " " + year
		}
	}
	return d.Name
}
