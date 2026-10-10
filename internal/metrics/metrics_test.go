package metrics

import (
	"bytes"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestExposition(t *testing.T) {
	r := &Registry{}
	c := r.NewCounter("jb_requests_total", "Requests.", "route", "code")
	c.Inc("/Items", "2xx")
	c.Add(2, "/Items", "2xx")
	c.Inc(`we"ird`, "5xx")
	h := r.NewHistogram("jb_seconds", "Latency.", []float64{0.1, 1}, "route")
	h.Observe(0.05, "/Items")
	h.Observe(0.5, "/Items")
	h.Observe(5, "/Items")
	r.NewGaugeFunc("jb_up", "Up.", []string{"upstream"}, func(emit func(float64, ...string)) {
		emit(1, "b")
		emit(0, "a")
	})
	var b bytes.Buffer
	r.Write(&b)
	want := `# HELP jb_requests_total Requests.
# TYPE jb_requests_total counter
jb_requests_total{route="/Items",code="2xx"} 3
jb_requests_total{route="we\"ird",code="5xx"} 1
# HELP jb_seconds Latency.
# TYPE jb_seconds histogram
jb_seconds_bucket{route="/Items",le="0.1"} 1
jb_seconds_bucket{route="/Items",le="1"} 2
jb_seconds_bucket{route="/Items",le="+Inf"} 3
jb_seconds_sum{route="/Items"} 5.55
jb_seconds_count{route="/Items"} 3
# HELP jb_up Up.
# TYPE jb_up gauge
jb_up{upstream="a"} 0
jb_up{upstream="b"} 1
`
	if b.String() != want {
		t.Errorf("got:\n%s\nwant:\n%s", b.String(), want)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	if !strings.HasPrefix(w.Header().Get("Content-Type"), "text/plain; version=0.0.4") || w.Body.String() != want {
		t.Errorf("served %q: %s", w.Header().Get("Content-Type"), w.Body.String())
	}
}

func TestLabelCountMismatchPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("no panic")
		}
	}()
	(&Registry{}).NewCounter("x", "x", "a").Inc()
}
