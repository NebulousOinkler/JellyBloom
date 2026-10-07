package identity

import (
	"encoding/json"
	"strconv"
	"strings"
	"unicode"

	"golang.org/x/text/runes"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"
)

// item is the slice of a BaseItemDto that identity needs.
type item struct {
	Upstream string `json:"-"`

	Id                string
	Type              string
	Name              string
	ProductionYear    int
	ProviderIds       map[string]string
	ParentId          string
	SeriesId          string
	SeasonId          string
	AlbumId           string
	AlbumArtist       string
	AlbumArtists      []struct{ Id string }
	IndexNumber       *int
	ParentIndexNumber *int
}

func parseItem(upstream string, dto json.RawMessage) (*item, error) {
	it := &item{Upstream: upstream}
	if err := json.Unmarshal(dto, it); err != nil {
		return nil, err
	}
	for _, p := range []*string{&it.Id, &it.ParentId, &it.SeriesId, &it.SeasonId, &it.AlbumId} {
		*p = NormalizeID(*p)
	}
	for i := range it.AlbumArtists {
		it.AlbumArtists[i].Id = NormalizeID(it.AlbumArtists[i].Id)
	}
	return it, nil
}

// ref identifies one upstream copy.
type ref struct{ upstream, id string }

func (it *item) ref() ref { return ref{it.Upstream, it.Id} }

// provider returns a provider ID, case-insensitively.
func (it *item) provider(name string) string {
	for k, v := range it.ProviderIds {
		if strings.EqualFold(k, name) {
			return strings.ToLower(strings.TrimSpace(v))
		}
	}
	return ""
}

// rule describes how an item type is matched across upstreams.
type rule struct {
	kind string
	// strong lists provider-ID names in preference order. Items sharing any
	// strong key are the same item.
	strong []string
	// weak builds a fallback key from metadata; parent resolves an upstream
	// reference (same upstream as the item) to its unified ID, or "".
	weak func(it *item, parent func(realID string) string) string
	// tier orders resolution: a weak key may only use parents from a lower tier.
	tier int
}

var rules = map[string]rule{
	"Movie":       {kind: "movie", strong: []string{"Tmdb", "Imdb"}, weak: titleYear},
	"Series":      {kind: "series", strong: []string{"Tvdb", "Tmdb", "Imdb"}, weak: titleYear},
	"BoxSet":      {kind: "boxset", strong: []string{"Tmdb"}, weak: title},
	"MusicArtist": {kind: "artist", strong: []string{"MusicBrainzArtist"}, weak: title},
	"MusicAlbum":  {kind: "album", strong: []string{"MusicBrainzReleaseGroup", "MusicBrainzAlbum"}, weak: albumKey},
	"MusicVideo":  {kind: "musicvideo", strong: []string{"Imdb"}, weak: titleYear},
	"Season":      {kind: "season", strong: []string{"Tvdb", "Tmdb"}, weak: seasonKey, tier: 1},
	"Episode":     {kind: "episode", strong: []string{"Tvdb", "Tmdb", "Imdb"}, weak: episodeKey, tier: 1},
	"Audio":       {kind: "audio", strong: []string{"MusicBrainzRecording", "MusicBrainzTrack"}, weak: trackKey, tier: 1},
}

// tiers is the number of resolution tiers in rules.
const tiers = 2

func ruleFor(typ string) (rule, bool) {
	r, ok := rules[typ]
	return r, ok
}

// keys returns the item's strong and weak identity keys. Items with no rule
// get a single upstream-unique strong key, so they are never merged.
func keys(it *item, parent func(string) string) (strong []string, weak string) {
	r, ok := ruleFor(it.Type)
	if !ok {
		return []string{uniqueKey(it)}, ""
	}
	for _, p := range r.strong {
		if v := it.provider(p); v != "" {
			strong = append(strong, r.kind+":"+strings.ToLower(p)+":"+v)
		}
	}
	if r.weak != nil {
		if w := r.weak(it, parent); w != "" {
			weak = r.kind + ":~" + w
		}
	}
	if len(strong) == 0 && weak == "" {
		return []string{uniqueKey(it)}, ""
	}
	return strong, weak
}

func uniqueKey(it *item) string { return "uniq:" + it.Upstream + ":" + strings.ToLower(it.Id) }

// providerOf returns the provider namespace of a strong key ("tmdb" in
// "movie:tmdb:603"), used to detect conflicting IDs.
func providerOf(key string) string {
	parts := strings.SplitN(key, ":", 3)
	if len(parts) < 3 {
		return ""
	}
	return parts[1]
}

// normTitle folds a title for comparison: accent-free lowercase letters and digits only.
func normTitle(s string) string {
	// Strip accents ("Björk" -> "Bjork"). Transformers are stateful, so build one per call.
	fold := transform.Chain(norm.NFKD, runes.Remove(runes.In(unicode.Mn)), norm.NFC)
	if f, _, err := transform.String(fold, s); err == nil {
		s = f
	}
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func title(it *item, _ func(string) string) string { return normTitle(it.Name) }

func titleYear(it *item, _ func(string) string) string {
	n := normTitle(it.Name)
	if n == "" || it.ProductionYear == 0 {
		return "" // a bare title is too ambiguous for films and shows
	}
	return n + ":" + strconv.Itoa(it.ProductionYear)
}

func albumKey(it *item, _ func(string) string) string {
	n, a := normTitle(it.Name), normTitle(it.AlbumArtist)
	if n == "" || a == "" {
		return ""
	}
	return a + ":" + n
}

func seasonKey(it *item, parent func(string) string) string {
	series := parent(it.SeriesId)
	if series == "" || it.IndexNumber == nil {
		return ""
	}
	return series + ":" + strconv.Itoa(*it.IndexNumber)
}

func episodeKey(it *item, parent func(string) string) string {
	series := parent(it.SeriesId)
	if series == "" || it.IndexNumber == nil || it.ParentIndexNumber == nil {
		return ""
	}
	return series + ":" + strconv.Itoa(*it.ParentIndexNumber) + ":" + strconv.Itoa(*it.IndexNumber)
}

func trackKey(it *item, parent func(string) string) string {
	album := parent(it.AlbumId)
	if album == "" {
		return ""
	}
	disc := 1
	if it.ParentIndexNumber != nil {
		disc = *it.ParentIndexNumber
	}
	if it.IndexNumber != nil {
		return album + ":" + strconv.Itoa(disc) + ":" + strconv.Itoa(*it.IndexNumber)
	}
	if n := normTitle(it.Name); n != "" {
		return album + ":" + n
	}
	return ""
}

// parents returns the upstream IDs an item hangs under in the hierarchy.
func (it *item) parents() []string {
	var out []string
	for _, p := range []string{it.ParentId, it.SeasonId, it.SeriesId, it.AlbumId} {
		if p != "" {
			out = append(out, p)
		}
	}
	for _, a := range it.AlbumArtists {
		if a.Id != "" {
			out = append(out, a.Id)
		}
	}
	return out
}
