package server

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/NebulousOinkler/JellyBloom/internal/identity"
	"github.com/NebulousOinkler/JellyBloom/internal/sources"
)

// streamRoutes registers the routes that serve files as stored (direct play
// with static=true, and downloads): the source registry relays them, failing
// over between identical copies mid-stream. Other stream requests (remuxes,
// transcodes) and anything else under these prefixes take the proxy path.
// Also the admin redundancy report.
func (s *Server) streamRoutes(rt *router) {
	for _, prefix := range []string{"/Videos", "/Audio"} {
		rt.handle(http.MethodGet, prefix+"/{id}/{file}", func(w http.ResponseWriter, r *http.Request) {
			file := strings.ToLower(r.PathValue("file"))
			if s.sources == nil || file != "stream" && !strings.HasPrefix(file, "stream.") || !strings.EqualFold(readParams(r).str("static"), "true") {
				s.fallback(w, r)
				return
			}
			s.authed(func(w http.ResponseWriter, r *http.Request, c *caller) {
				kind := "/Videos/"
				if strings.EqualFold(strings.Split(strings.Trim(r.URL.Path, "/"), "/")[0], "audio") {
					kind = "/Audio/"
				}
				s.relayFile(w, r, c, func(src sources.Source) (string, url.Values) {
					return kind + src.Item + "/" + r.PathValue("file"), url.Values{"Static": {"true"}, "MediaSourceId": {src.Media}}
				})
			})(w, r)
		})
	}
	rt.handle(http.MethodGet, "/Items/{id}/Download", func(w http.ResponseWriter, r *http.Request) {
		if s.sources == nil {
			s.fallback(w, r)
			return
		}
		s.authed(func(w http.ResponseWriter, r *http.Request, c *caller) {
			s.relayFile(w, r, c, func(src sources.Source) (string, url.Values) {
				return "/Items/" + src.Item + "/Download", url.Values{"MediaSourceId": {src.Media}}
			})
		})(w, r)
	})

	rt.handle(http.MethodGet, "/JellyBloom/Report", s.authed(func(w http.ResponseWriter, r *http.Request, c *caller) {
		if !c.user.Admin {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		if s.sources == nil {
			http.NotFound(w, r)
			return
		}
		rep, err := s.sources.Report(r.Context())
		if err != nil {
			s.internal(w, err)
			return
		}
		writeJSON(w, rep)
	}))
}

// relayFile serves an item's file through the source registry. The copies
// are those PlaybackInfo offered for the requested version in this play
// session; failing that (a client that skipped PlaybackInfo, or a restart),
// the version's or item's copies.
func (s *Server) relayFile(w http.ResponseWriter, r *http.Request, c *caller, target func(sources.Source) (string, url.Values)) {
	ctx := r.Context()
	p := readParams(r)
	item := identity.NormalizeID(r.PathValue("id"))
	if !s.catalog.Known(item) {
		http.NotFound(w, r)
		return
	}
	version := identity.NormalizeID(p.str("mediaSourceId"))
	if version == "" {
		version = item
	}
	psid := identity.NormalizeID(p.str("playSessionId"))
	pc := proxyCaller(c)
	srcs := s.sources.VersionSources(psid, c.user.ID, pc.DeviceID, item, version)
	one := srcs != nil
	if srcs == nil {
		var err error
		if srcs, err = s.fileSources(ctx, item, version); err != nil {
			http.NotFound(w, r)
			return
		}
		one = version != item // a version alias lists one version's copies
	}
	s.sources.Relay(w, r, sources.Stream{
		Sources: srcs, OneVersion: one, Session: psid, User: c.user.ID, Device: pc.DeviceID, Item: item,
		Open: func(ctx context.Context, src sources.Source, method string, hdr http.Header) (*http.Response, error) {
			path, q := target(src)
			return s.proxy.Open(ctx, src.Upstream, pc, method, path, q, hdr)
		},
	})
}

var errNoCopies = errors.New("no copies")

// fileSources lists the copies of an item (version is the item itself) or
// of one of its versions (a version ID from PlaybackInfo).
func (s *Server) fileSources(ctx context.Context, item, version string) ([]sources.Source, error) {
	copies := s.catalog.Copies(item)
	var out []sources.Source
	if version == item {
		for _, rf := range copies {
			id := identity.NormalizeID(rf.ID)
			out = append(out, sources.Source{Upstream: rf.Upstream, Item: id, Media: id})
		}
	} else if s.opts.Resolver != nil {
		v, err := s.opts.Resolver.Lookup(ctx, version)
		if err != nil || !v.Lazy {
			return nil, errNoCopies
		}
		for _, rf := range v.Refs {
			// The media source belongs to the copy whose ID it shares (a
			// primary media source), else to the item's copy on its upstream.
			ms, owner := identity.NormalizeID(rf.ID), ""
			for _, cp := range copies {
				id := identity.NormalizeID(cp.ID)
				if cp.Upstream == rf.Upstream && (owner == "" || id == ms) {
					owner = id
				}
			}
			if owner != "" {
				out = append(out, sources.Source{Upstream: rf.Upstream, Item: owner, Media: ms})
			}
		}
	}
	if len(out) == 0 {
		return nil, errNoCopies
	}
	return out, nil
}
