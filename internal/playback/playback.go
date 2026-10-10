// Package playback answers PlaybackInfo: it asks every upstream holding a
// copy of the item, groups their media sources into versions (distinct
// encodes), and returns one MediaSource per version. Version and play
// session IDs become aliases over the upstream sources they stand for, so the
// generic proxy routes the following stream, HLS, subtitle, and progress
// requests to the right upstream with the right real IDs.
package playback

import (
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/NebulousOinkler/JellyBloom/internal/identity"
	"github.com/NebulousOinkler/JellyBloom/internal/proxy"
	"github.com/NebulousOinkler/JellyBloom/internal/rewrite"
)

// Proxy is the upstream access playback needs; *proxy.Proxy implements it.
type Proxy interface {
	Call(ctx context.Context, r *http.Request, upstream string, c proxy.Caller, method, path string,
		query url.Values, body []byte) (*http.Response, *rewrite.Replacer, error)
	UpstreamUser(ctx context.Context, upstream string) string
	Upstreams() []string
}

// Resolver is the identity API playback needs; *identity.Resolver implements it.
type Resolver interface {
	Lookup(ctx context.Context, id string) (*identity.Unified, error)
	SetAliases(ctx context.Context, aliases []identity.Alias) error
}

// Handler serves PlaybackInfo.
type Handler struct {
	px      Proxy
	res     Resolver
	log     *slog.Logger
	timeout time.Duration
}

// New creates a Handler.
func New(px Proxy, res Resolver, log *slog.Logger) *Handler {
	if log == nil {
		log = slog.Default()
	}
	return &Handler{px: px, res: res, log: log, timeout: 20 * time.Second}
}

// source is one upstream copy of an item.
type source struct {
	up, item, ms string // upstream, real item ID, real media source ID
}

// answer is one upstream's PlaybackInfo response.
type answer struct {
	up      string
	item    string
	psid    string
	sources []map[string]any
	rep     *rewrite.Replacer
	errCode string
}

// candidate is one upstream media source.
type candidate struct {
	src source
	ms  map[string]any
	ans *answer
}

// version is a distinct encode and the sources that have it, best first.
type version struct {
	key   string
	id    string
	cands []candidate
}

func newPlaySessionID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func getFold(m map[string]any, key string) (string, any, bool) {
	for k, v := range m {
		if strings.EqualFold(k, key) {
			return k, v, true
		}
	}
	return "", nil, false
}

func queryFold(q url.Values, key string) string {
	for k, vs := range q {
		if strings.EqualFold(k, key) && len(vs) > 0 {
			return vs[0]
		}
	}
	return ""
}

func setQueryFold(q url.Values, key, val string) {
	for k := range q {
		if strings.EqualFold(k, key) {
			delete(q, k)
		}
	}
	if val != "" {
		q.Set(key, val)
	}
}

func decode(b []byte) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return nil, err
	}
	return m, nil
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.Encode(v)
}

// PlaybackInfo handles GET/POST /Items/{id}/PlaybackInfo.
func (h *Handler) PlaybackInfo(w http.ResponseWriter, r *http.Request, c proxy.Caller) {
	ctx := r.Context()
	itemID := identity.NormalizeID(r.PathValue("id"))
	u, err := h.res.Lookup(ctx, itemID)
	if errors.Is(err, identity.ErrNotFound) || err == nil && (!u.Visible || u.Lazy) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		h.fail(w, err)
		return
	}

	var body map[string]any
	if r.Method == http.MethodPost && r.ContentLength != 0 {
		raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err == nil && len(bytes.TrimSpace(raw)) > 0 {
			body, err = decode(raw)
		}
		if err != nil {
			http.Error(w, "bad request body", http.StatusBadRequest)
			return
		}
	}

	// The copies to ask, preferred upstream first; narrowed when the client
	// asks for one version.
	order := map[string]int{}
	for i, name := range h.px.Upstreams() {
		order[name] = i
	}
	var srcs []source
	for _, rf := range u.Refs {
		if rf.Source {
			srcs = append(srcs, source{up: rf.Upstream, item: identity.NormalizeID(rf.ID)})
		}
	}
	slices.SortStableFunc(srcs, func(a, b source) int { return order[a.up] - order[b.up] })

	requested := queryFold(r.URL.Query(), "MediaSourceId")
	if _, v, ok := getFold(body, "MediaSourceId"); ok && requested == "" {
		requested, _ = v.(string)
	}
	requested = identity.NormalizeID(requested)
	if requested != "" && requested != itemID {
		v, err := h.res.Lookup(ctx, requested)
		if err != nil || !v.Lazy {
			http.NotFound(w, r)
			return
		}
		var narrowed []source
		for _, rf := range v.Refs {
			for _, s := range srcs {
				if s.up == rf.Upstream {
					s.ms = identity.NormalizeID(rf.ID)
					narrowed = append(narrowed, s)
				}
			}
		}
		srcs = narrowed
	}
	if len(srcs) == 0 {
		http.NotFound(w, r)
		return
	}

	answers := h.ask(r, c, srcs, body)
	h.respond(w, r, itemID, answers, requested != "")
}

// ask sends PlaybackInfo to every source's upstream concurrently.
func (h *Handler) ask(r *http.Request, c proxy.Caller, srcs []source, body map[string]any) []*answer {
	ctx, cancel := context.WithTimeout(r.Context(), h.timeout)
	defer cancel()
	out := make([]*answer, len(srcs))
	var wg sync.WaitGroup
	for i, s := range srcs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a, err := h.askOne(ctx, r, c, s, body)
			if err != nil {
				h.log.Warn("playback info failed", "upstream", s.up, "err", err)
				return
			}
			out[i] = a
		}()
	}
	wg.Wait()
	return slices.DeleteFunc(out, func(a *answer) bool { return a == nil })
}

func (h *Handler) askOne(ctx context.Context, r *http.Request, c proxy.Caller, s source, body map[string]any) (*answer, error) {
	upUser := h.px.UpstreamUser(ctx, s.up)
	q := r.URL.Query()
	setQueryFold(q, "userId", upUser)
	setQueryFold(q, "MediaSourceId", s.ms)
	for _, k := range []string{"LiveStreamId"} {
		setQueryFold(q, k, "")
	}

	var payload []byte
	if body != nil {
		b := make(map[string]any, len(body))
		for k, v := range body {
			b[k] = v
		}
		for _, k := range []string{"UserId", "MediaSourceId", "LiveStreamId"} {
			if fk, _, ok := getFold(b, k); ok {
				delete(b, fk)
			}
		}
		if upUser != "" {
			b["UserId"] = upUser
		}
		if s.ms != "" {
			b["MediaSourceId"] = s.ms
		}
		payload, _ = json.Marshal(b)
	}

	resp, rep, err := h.px.Call(ctx, r, s.up, c, r.Method, "/Items/"+s.item+"/PlaybackInfo", q, payload)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, bytes.TrimSpace(raw[:min(len(raw), 200)]))
	}
	m, err := decode(raw)
	if err != nil {
		return nil, err
	}
	a := &answer{up: s.up, item: s.item, rep: rep}
	a.psid, _ = m["PlaySessionId"].(string)
	a.errCode, _ = m["ErrorCode"].(string)
	if list, ok := m["MediaSources"].([]any); ok {
		for _, e := range list {
			if ms, ok := e.(map[string]any); ok {
				a.sources = append(a.sources, ms)
			}
		}
	}
	return a, nil
}

func num(v any) float64 {
	switch t := v.(type) {
	case json.Number:
		f, _ := t.Float64()
		return f
	case float64:
		return t
	}
	return 0
}

func str(m map[string]any, k string) string {
	s, _ := m[k].(string)
	return s
}

func streams(ms map[string]any) []map[string]any {
	var out []map[string]any
	if list, ok := ms["MediaStreams"].([]any); ok {
		for _, e := range list {
			if s, ok := e.(map[string]any); ok {
				out = append(out, s)
			}
		}
	}
	return out
}

func videoStream(ms map[string]any) map[string]any {
	for _, s := range streams(ms) {
		if str(s, "Type") == "Video" {
			return s
		}
	}
	return nil
}

// versionKey identifies an encode: copies with equal keys are the same file.
// Phase 8 adds partial content hashes; size, runtime, and stream layout
// already separate real-world encodes.
func versionKey(ms map[string]any) string {
	var parts []string
	parts = append(parts, strings.ToLower(str(ms, "Container")))
	size := int64(num(ms["Size"]))
	parts = append(parts, strconv.FormatInt(size, 10))
	parts = append(parts, strconv.FormatInt(int64(num(ms["RunTimeTicks"]))/10_000_000, 10))
	if size == 0 {
		parts = append(parts, strconv.FormatInt(int64(num(ms["Bitrate"])), 10))
	}
	for _, s := range streams(ms) {
		switch str(s, "Type") {
		case "Video":
			parts = append(parts, fmt.Sprintf("v:%s:%dx%d", strings.ToLower(str(s, "Codec")), int(num(s["Width"])), int(num(s["Height"]))))
		case "Audio":
			parts = append(parts, fmt.Sprintf("a:%s:%d", strings.ToLower(str(s, "Codec")), int(num(s["Channels"]))))
		}
	}
	return strings.Join(parts, "|")
}

// label names a version by its encode, never by where it lives.
func label(ms map[string]any) string {
	if v := videoStream(ms); v != nil {
		res := ""
		switch h := int(num(v["Height"])); {
		case h >= 2000:
			res = "4K"
		case h > 0:
			res = strconv.Itoa(h) + "p"
		}
		parts := []string{res, strings.ToUpper(str(v, "Codec"))}
		if r := str(v, "VideoRange"); r != "" && r != "SDR" {
			parts = append(parts, r)
		}
		return strings.TrimSpace(strings.Join(parts, " "))
	}
	l := strings.ToUpper(str(ms, "Container"))
	if br := int(num(ms["Bitrate"])); br > 0 {
		l += fmt.Sprintf(" %dkbps", br/1000)
	}
	return l
}

func truthy(v any) bool { b, _ := v.(bool); return b }

// rank orders versions for the client's device: direct play, then direct
// stream, then transcoding; then higher resolution and bitrate.
func rankVersions(vs []*version) {
	score := func(ms map[string]any) int {
		switch {
		case truthy(ms["SupportsDirectPlay"]):
			return 3
		case truthy(ms["SupportsDirectStream"]):
			return 2
		case truthy(ms["SupportsTranscoding"]):
			return 1
		}
		return 0
	}
	height := func(ms map[string]any) int {
		if v := videoStream(ms); v != nil {
			return int(num(v["Height"]))
		}
		return 0
	}
	slices.SortStableFunc(vs, func(a, b *version) int {
		x, y := a.cands[0].ms, b.cands[0].ms
		return cmp.Or(score(y)-score(x), height(y)-height(x), cmp.Compare(num(y["Bitrate"]), num(x["Bitrate"])))
	})
}

func (h *Handler) respond(w http.ResponseWriter, r *http.Request, itemID string, answers []*answer, narrowed bool) {
	ctx := r.Context()
	byKey := map[string]*version{}
	var versions []*version
	for _, a := range answers {
		for _, ms := range a.sources {
			c := candidate{src: source{up: a.up, item: a.item, ms: identity.NormalizeID(str(ms, "Id"))}, ms: ms, ans: a}
			if c.src.ms == "" {
				c.src.ms = a.item
			}
			k := versionKey(ms)
			v := byKey[k]
			if v == nil {
				v = &version{key: k}
				byKey[k] = v
				versions = append(versions, v)
			}
			v.cands = append(v.cands, c)
		}
	}
	if len(versions) == 0 {
		for _, a := range answers {
			if a.errCode != "" {
				writeJSON(w, map[string]any{"MediaSources": []any{}, "ErrorCode": a.errCode, "PlaySessionId": newPlaySessionID()})
				return
			}
		}
		http.Error(w, "no playable source", http.StatusBadGateway)
		return
	}
	rankVersions(versions)

	// A single version whose sources are their items' primary media source
	// keeps the item's ID as its media source ID, as Jellyfin does — unless
	// the client asked for a version by its derived ID, which must stay stable.
	single := !narrowed && len(versions) == 1 && !slices.ContainsFunc(versions[0].cands, func(c candidate) bool { return c.src.ms != c.src.item })
	var aliases []identity.Alias
	for _, v := range versions {
		if single {
			v.id = itemID
			continue
		}
		v.id = identity.DeriveID("version:" + itemID + ":" + v.key)
		a := identity.Alias{ID: v.id}
		for _, c := range v.cands {
			a.Refs = append(a.Refs, identity.Ref{Upstream: c.src.up, ID: c.src.ms})
		}
		aliases = append(aliases, a)
	}
	psid := newPlaySessionID()
	ps := identity.Alias{ID: psid}
	for _, a := range answers {
		if a.psid != "" {
			ps.Refs = append(ps.Refs, identity.Ref{Upstream: a.up, ID: a.psid})
		}
	}
	if len(ps.Refs) > 0 {
		aliases = append(aliases, ps)
	}
	if err := h.res.SetAliases(ctx, aliases); err != nil {
		h.fail(w, err)
		return
	}

	// The aliases just set map each upstream's media source and play session
	// IDs to the version and JellyBloom play session, so the upstream's own
	// replacer rewrites them. A primary media source shares its item's ID, so
	// its MediaSourceId= comes out as the item ID and is pointed at the
	// version here.
	labels := map[string]int{}
	var out []any
	for _, v := range versions {
		top := v.cands[0]
		d := top.ans.rep.Value(top.ms).(map[string]any)
		d["Id"] = v.id
		if top.src.ms == top.src.item && v.id != itemID {
			msParam := regexp.MustCompile(`(?i)(mediasourceid=)` + itemID)
			for _, k := range []string{"TranscodingUrl", "DirectStreamUrl"} {
				if u, ok := d[k].(string); ok {
					d[k] = msParam.ReplaceAllString(u, "${1}"+v.id)
				}
			}
		}
		if len(versions) > 1 {
			l := label(top.ms)
			labels[l]++
			if labels[l] > 1 {
				l = fmt.Sprintf("%s (%d)", l, labels[l])
			}
			d["Name"] = l
		}
		out = append(out, d)
	}
	h.log.Debug("playback info", "item", itemID, "versions", len(versions), "narrowed", narrowed)
	writeJSON(w, map[string]any{"MediaSources": out, "PlaySessionId": psid})
}

func (h *Handler) fail(w http.ResponseWriter, err error) {
	h.log.Error("playback info", "err", err)
	http.Error(w, "Internal Server Error", http.StatusInternalServerError)
}
