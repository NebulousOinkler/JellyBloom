// Command faultproxy sits between JellyBloom and the rig's Jellyfin servers
// and injects faults on demand, so failover can be tested at will: latency,
// throttled media, media bodies that break off or stall, error statuses on
// media, or a full outage. For the test rig only; it binds to loopback.
//
//	faultproxy -listen 127.0.0.1:8211=http://127.0.0.1:8201 -listen 127.0.0.1:8212=http://127.0.0.1:8202
//
// Each listener has its own controls under /__faults:
//
//	GET    /__faults  current faults and counters
//	POST   /__faults  set faults (JSON; omitted fields are cleared)
//	DELETE /__faults  clear faults and counters
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Faults are what a listener injects. Media requests are file streams,
// downloads, and HLS (see mediaPath); other faults apply to every request.
type Faults struct {
	Down         bool  `json:"down"`          // close every connection at once
	LatencyMs    int   `json:"latency_ms"`    // delay before each request
	ThrottleKBps int   `json:"throttle_kbps"` // media bodies at most this many kB/s
	CutAfter     int64 `json:"cut_after"`     // media bodies break off after this many bytes
	StallAfter   int64 `json:"stall_after"`   // media bodies stop sending after this many bytes
	Status       int   `json:"status"`        // media requests get this status
}

// Counters show which listener served what.
type Counters struct {
	Requests      int64 `json:"requests"`
	MediaRequests int64 `json:"media_requests"`
	MediaBytes    int64 `json:"media_bytes"`
}

var mediaPath = regexp.MustCompile(`(?i)^/(videos|audio)/[^/]+/(stream|universal|master\.m3u8|main\.m3u8|live\.m3u8|hls1?/)|^/items/[^/]+/download`)

type listener struct {
	mu     sync.Mutex
	faults Faults
	count  Counters
	proxy  *httputil.ReverseProxy
}

func (l *listener) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/__faults" {
		l.control(w, r)
		return
	}
	l.mu.Lock()
	f := l.faults
	l.count.Requests++
	media := mediaPath.MatchString(r.URL.Path)
	if media {
		l.count.MediaRequests++
	}
	l.mu.Unlock()

	if f.Down {
		if hj, ok := w.(http.Hijacker); ok {
			if c, _, err := hj.Hijack(); err == nil {
				c.Close()
				return
			}
		}
		panic(http.ErrAbortHandler)
	}
	if f.LatencyMs > 0 {
		select {
		case <-time.After(time.Duration(f.LatencyMs) * time.Millisecond):
		case <-r.Context().Done():
			return
		}
	}
	if media && f.Status != 0 {
		http.Error(w, "injected fault", f.Status)
		return
	}
	if media {
		r = r.WithContext(context.WithValue(r.Context(), mediaKey{}, f))
	}
	l.proxy.ServeHTTP(w, r)
}

type mediaKey struct{}

func (l *listener) control(w http.ResponseWriter, r *http.Request) {
	l.mu.Lock()
	defer l.mu.Unlock()
	switch r.Method {
	case http.MethodPost:
		var f Faults
		if err := json.NewDecoder(r.Body).Decode(&f); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		l.faults = f
		log.Printf("%s faults: %+v", r.Host, f)
	case http.MethodDelete:
		l.faults, l.count = Faults{}, Counters{}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"faults": l.faults, "counters": l.count})
}

// faultyBody applies media faults to a response body and counts its bytes.
type faultyBody struct {
	io.ReadCloser
	l     *listener
	f     Faults
	ctx   context.Context
	sent  int64
	start time.Time
}

var errCut = errors.New("faultproxy: body cut")

func (b *faultyBody) Read(p []byte) (int, error) {
	if b.f.CutAfter > 0 && b.sent >= b.f.CutAfter {
		return 0, errCut
	}
	if b.f.StallAfter > 0 && b.sent >= b.f.StallAfter {
		<-b.ctx.Done()
		return 0, b.ctx.Err()
	}
	if lim := b.f.CutAfter; lim > 0 && int64(len(p)) > lim-b.sent {
		p = p[:lim-b.sent]
	}
	if lim := b.f.StallAfter; lim > 0 && int64(len(p)) > lim-b.sent {
		p = p[:lim-b.sent]
	}
	if b.f.ThrottleKBps > 0 && len(p) > 16<<10 {
		p = p[:16<<10]
	}
	n, err := b.ReadCloser.Read(p)
	b.sent += int64(n)
	b.l.mu.Lock()
	b.l.count.MediaBytes += int64(n)
	b.l.mu.Unlock()
	if b.f.ThrottleKBps > 0 {
		due := b.start.Add(time.Duration(float64(b.sent) / float64(b.f.ThrottleKBps*1000) * float64(time.Second)))
		if d := time.Until(due); d > 0 {
			select {
			case <-time.After(d):
			case <-b.ctx.Done():
			}
		}
	}
	return n, err
}

type listenFlags []string

func (f *listenFlags) String() string     { return strings.Join(*f, ",") }
func (f *listenFlags) Set(v string) error { *f = append(*f, v); return nil }

func main() {
	var listens listenFlags
	flag.Var(&listens, "listen", "ADDR=TARGET_URL (repeatable); ADDR must be a loopback address")
	flag.Parse()
	if len(listens) == 0 {
		log.Fatal("no -listen given")
	}
	var wg sync.WaitGroup
	for _, spec := range listens {
		addr, target, ok := strings.Cut(spec, "=")
		u, err := url.Parse(target)
		if !ok || err != nil {
			log.Fatalf("bad -listen %q", spec)
		}
		if host, _, err := net.SplitHostPort(addr); err != nil || !net.ParseIP(host).IsLoopback() {
			log.Fatalf("listen address %q must be loopback", addr)
		}
		l := &listener{}
		l.proxy = &httputil.ReverseProxy{
			Rewrite:       func(pr *httputil.ProxyRequest) { pr.SetURL(u); pr.Out.Host = pr.In.Host },
			FlushInterval: -1,
			ModifyResponse: func(resp *http.Response) error {
				if f, ok := resp.Request.Context().Value(mediaKey{}).(Faults); ok {
					resp.Body = &faultyBody{ReadCloser: resp.Body, l: l, f: f, ctx: resp.Request.Context(), start: time.Now()}
				}
				return nil
			},
			ErrorLog: log.New(io.Discard, "", 0), // cut bodies are the point
		}
		srv := &http.Server{Addr: addr, Handler: l, ReadHeaderTimeout: 10 * time.Second}
		wg.Add(1)
		go func() {
			defer wg.Done()
			log.Printf("faultproxy %s -> %s", addr, target)
			if err := srv.ListenAndServe(); err != nil {
				log.Fatal(err)
			}
		}()
	}
	wg.Wait()
}
