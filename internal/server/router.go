package server

import (
	"net/http"
	"strings"
)

// router matches routes case-insensitively, as Jellyfin does: clients mix
// "/Users/Me" and "/users/me". Path parameters keep their original case and
// are available via r.PathValue. A trailing "{name...}" segment matches the rest.
type router struct {
	routes   []route
	fallback http.Handler
}

type route struct {
	method string
	segs   []string // lowercased literals, or "{name}" / "{name...}"
	h      http.Handler
}

func split(p string) []string {
	p = strings.Trim(p, "/")
	if p == "" {
		return nil
	}
	return strings.Split(p, "/")
}

// handle registers h for method ("" for any) and pattern.
func (rt *router) handle(method, pattern string, h http.HandlerFunc) {
	segs := split(pattern)
	for i, s := range segs {
		if !strings.HasPrefix(s, "{") {
			segs[i] = strings.ToLower(s)
		}
	}
	rt.routes = append(rt.routes, route{method: method, segs: segs, h: h})
}

func (rt *router) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	segs := split(r.URL.Path)
	for _, ro := range rt.routes {
		if ro.method != "" && ro.method != r.Method && !(ro.method == http.MethodGet && r.Method == http.MethodHead) {
			continue
		}
		params, ok := ro.match(segs)
		if !ok {
			continue
		}
		for k, v := range params {
			r.SetPathValue(k, v)
		}
		ro.h.ServeHTTP(w, r)
		return
	}
	// Unmatched paths and methods go to the fallback, which applies the
	// forwarding policy.
	rt.fallback.ServeHTTP(w, r)
}

func (ro route) match(segs []string) (map[string]string, bool) {
	params := map[string]string{}
	for i, s := range ro.segs {
		if strings.HasSuffix(s, "...}") {
			params[s[1:len(s)-4]] = strings.Join(segs[i:], "/")
			return params, true
		}
		if i >= len(segs) {
			return nil, false
		}
		if strings.HasPrefix(s, "{") {
			params[s[1:len(s)-1]] = segs[i]
			continue
		}
		if s != strings.ToLower(segs[i]) {
			return nil, false
		}
	}
	return params, len(segs) == len(ro.segs)
}
