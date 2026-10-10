// Package rewrite substitutes GUIDs and literal strings in proxied bodies,
// URLs, and headers, so clients only ever see JellyBloom IDs, URLs, and tokens.
package rewrite

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"
)

// guidRe matches a GUID in dashed or 32-hex form. Boundaries are checked
// separately (RE2 has no lookaround) so longer hex runs, such as 64-hex hashes,
// never match partially.
var guidRe = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}|[0-9a-fA-F]{32}`)

func isWordByte(b byte) bool {
	return b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z'
}

// Normalize returns the 32-hex lowercase form of a GUID match.
func Normalize(guid string) string { return strings.ToLower(strings.ReplaceAll(guid, "-", "")) }

func dashed(id string) string {
	return id[:8] + "-" + id[8:12] + "-" + id[12:16] + "-" + id[16:20] + "-" + id[20:]
}

// EachGUID calls fn with the normalized form of every standalone GUID in s.
func EachGUID(s string, fn func(norm string)) {
	for _, m := range guidRe.FindAllStringIndex(s, -1) {
		if (m[0] > 0 && isWordByte(s[m[0]-1])) || (m[1] < len(s) && isWordByte(s[m[1]])) {
			continue
		}
		fn(Normalize(s[m[0]:m[1]]))
	}
}

// ReplaceGUIDs replaces every standalone GUID in s for which fn returns ok.
// fn gets and returns 32-hex lowercase IDs; dashed GUIDs stay dashed.
func ReplaceGUIDs(s string, fn func(norm string) (string, bool)) string {
	idx := guidRe.FindAllStringIndex(s, -1)
	if idx == nil {
		return s
	}
	var b strings.Builder
	last := 0
	for _, m := range idx {
		if (m[0] > 0 && isWordByte(s[m[0]-1])) || (m[1] < len(s) && isWordByte(s[m[1]])) {
			continue
		}
		orig := s[m[0]:m[1]]
		repl, ok := fn(Normalize(orig))
		if !ok || len(repl) != 32 {
			continue
		}
		if len(orig) == 36 {
			repl = dashed(repl)
		}
		b.WriteString(s[last:m[0]])
		b.WriteString(repl)
		last = m[1]
	}
	if last == 0 {
		return s
	}
	b.WriteString(s[last:])
	return b.String()
}

// Replacer rewrites one response or request.
type Replacer struct {
	// GUID maps a normalized GUID to its replacement; ok=false leaves it.
	GUID func(norm string) (string, bool)
	// Literals are old/new pairs applied after GUID replacement, e.g. the
	// upstream base URL and token. Tokens look like GUIDs, so GUID must
	// leave the old token alone; the new token is never seen by GUID.
	Literals []string
}

// String rewrites s.
func (r *Replacer) String(s string) string {
	if r.GUID != nil {
		s = ReplaceGUIDs(s, r.GUID)
	}
	for i := 0; i+1 < len(r.Literals); i += 2 {
		if r.Literals[i] != "" {
			s = strings.ReplaceAll(s, r.Literals[i], r.Literals[i+1])
		}
	}
	return s
}

// Text rewrites a non-JSON text body (m3u8, HTML, plain text).
func (r *Replacer) Text(b []byte) []byte { return []byte(r.String(string(b))) }

// skipKey reports JSON keys whose values are content hashes, not IDs: image
// tags, etags, and blurhash maps (keyed by image tag). Rewriting them would
// mint pointless IDs and break client image caching.
func skipKey(k string) bool {
	return k == "Etag" || k == "BlurHashes" || strings.HasSuffix(k, "Tag") || strings.HasSuffix(k, "Tags")
}

// JSON rewrites a JSON body structurally, skipping hash-valued keys. Invalid
// JSON falls back to Text.
func (r *Replacer) JSON(b []byte) []byte {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil || dec.More() {
		return r.Text(b)
	}
	v = r.walk(v)
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return r.Text(b)
	}
	return bytes.TrimSuffix(out.Bytes(), []byte("\n"))
}

func (r *Replacer) walk(v any) any {
	switch t := v.(type) {
	case string:
		return r.String(t)
	case []any:
		for i := range t {
			t[i] = r.walk(t[i])
		}
		return t
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			if skipKey(k) {
				out[k] = val
				continue
			}
			out[r.String(k)] = r.walk(val)
		}
		return out
	default:
		return v
	}
}

// IsText reports whether a Content-Type carries a body worth rewriting.
func IsText(contentType string) bool {
	ct := strings.ToLower(contentType)
	for _, s := range []string{"json", "text/", "mpegurl", "javascript", "xml"} {
		if strings.Contains(ct, s) {
			return true
		}
	}
	return false
}

// IsJSON reports whether a Content-Type is JSON.
func IsJSON(contentType string) bool { return strings.Contains(strings.ToLower(contentType), "json") }

// Template replaces every standalone GUID in s with "{id}", for route
// matching and logging: "/Items/0123…/Images" -> "/Items/{id}/Images".
func Template(s string) string {
	idx := guidRe.FindAllStringIndex(s, -1)
	var b strings.Builder
	last := 0
	for _, m := range idx {
		if (m[0] > 0 && isWordByte(s[m[0]-1])) || (m[1] < len(s) && isWordByte(s[m[1]])) {
			continue
		}
		b.WriteString(s[last:m[0]])
		b.WriteString("{id}")
		last = m[1]
	}
	b.WriteString(s[last:])
	return b.String()
}
