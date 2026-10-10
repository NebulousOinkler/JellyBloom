package server

import (
	"bufio"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/NebulousOinkler/JellyBloom/internal/metrics"
	"github.com/NebulousOinkler/JellyBloom/internal/rewrite"
)

var (
	httpRequests = metrics.Default.NewCounter("jellybloom_http_requests_total",
		"Client requests, by route (the handler's pattern, or fallback) and status class.", "route", "code")
	httpLatency = metrics.Default.NewHistogram("jellybloom_http_response_seconds",
		"Time until JellyBloom started answering a client request (streams then run as long as playback).",
		metrics.DurationBuckets, "route")
	httpBytes = metrics.Default.NewCounter("jellybloom_http_response_bytes_total",
		"Response body bytes sent to clients, by route.", "route")
)

// observed records a request's route, status, size, and timing for the
// access log and metrics. It keeps Flush, Hijack (WebSockets), and Unwrap
// (http.ResponseController) working.
type observed struct {
	http.ResponseWriter
	route  string
	status int
	bytes  int64
	start  time.Time
	first  time.Duration // until the response started
}

func (o *observed) WriteHeader(code int) {
	if o.status == 0 {
		o.status, o.first = code, time.Since(o.start)
	}
	o.ResponseWriter.WriteHeader(code)
}

func (o *observed) Write(b []byte) (int, error) {
	if o.status == 0 {
		o.status, o.first = http.StatusOK, time.Since(o.start)
	}
	n, err := o.ResponseWriter.Write(b)
	o.bytes += int64(n)
	return n, err
}

func (o *observed) Flush() {
	if f, ok := o.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (o *observed) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := o.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("hijacking not supported")
	}
	if o.status == 0 {
		o.status, o.first = http.StatusSwitchingProtocols, time.Since(o.start)
	}
	return h.Hijack()
}

func (o *observed) Unwrap() http.ResponseWriter { return o.ResponseWriter }

// setRoute is how the router names the handler that served the request.
func (o *observed) setRoute(pattern string) { o.route = pattern }

// observe wraps the server: every request is counted and timed, and logged
// at debug level (5xx at warn) with its path's IDs templated and no query
// string, since queries carry tokens.
func (s *Server) observe(w http.ResponseWriter, r *http.Request) {
	o := &observed{ResponseWriter: w, route: "fallback", start: time.Now()}
	s.router.ServeHTTP(o, r)
	if o.status == 0 {
		o.status, o.first = http.StatusOK, time.Since(o.start)
	}
	class := strconv.Itoa(o.status/100) + "xx"
	httpRequests.Inc(o.route, class)
	httpLatency.Observe(o.first.Seconds(), o.route)
	httpBytes.Add(float64(o.bytes), o.route)
	level := slog.LevelDebug
	if o.status >= 500 {
		level = slog.LevelWarn
	}
	if s.log.Enabled(r.Context(), level) {
		s.log.Log(r.Context(), level, "request", "method", r.Method, "path", rewrite.Template(r.URL.Path), "route", o.route,
			"status", o.status, "bytes", o.bytes, "first_byte_ms", o.first.Milliseconds(),
			"ms", time.Since(o.start).Milliseconds(), "client", r.Header.Get("User-Agent"), "remote", r.RemoteAddr)
	}
}

// SocketClients is the number of connected client sockets.
func (s *Server) SocketClients() int { return s.hub.count() }
