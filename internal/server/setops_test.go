package server

import (
	"fmt"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/NebulousOinkler/JellyBloom/internal/identity"
)

// setData has movies on a only, b only, and both; a series on both whose
// episode 1 is on both, episode 2 on a only, and episode 3 on b only; and a
// music library on b only. Each movie has its own genre.
func setData() map[string]library {
	movie := func(id, name, tmdb, genre, view string) map[string]any {
		return map[string]any{"Id": id, "Type": "Movie", "Name": name, "ParentId": view, "MediaType": "Video",
			"ProviderIds": map[string]string{"Tmdb": tmdb}, "Genres": []string{genre},
			"GenreItems": []map[string]any{{"Name": genre, "Id": hexID("9e" + genre[:2])}}, "DateCreated": "2026-01-01T00:00:00Z"}
	}
	show := func(up string) []map[string]any {
		series, season := hexID("5e"+up), hexID("5a"+up)
		ep := func(n int) map[string]any {
			return map[string]any{"Id": hexID(fmt.Sprintf("e%s%d", up, n)), "Type": "Episode", "Name": fmt.Sprintf("Episode %d", n),
				"ParentId": season, "SeriesId": series, "SeasonId": season, "SeriesName": "Show", "ParentIndexNumber": 1,
				"IndexNumber": n, "MediaType": "Video"}
		}
		items := []map[string]any{
			{"Id": series, "Type": "Series", "Name": "Show", "ParentId": hexID("vs" + up), "IsFolder": true, "ProviderIds": map[string]string{"Tvdb": "10"}},
			{"Id": season, "Type": "Season", "Name": "Season 1", "ParentId": series, "SeriesId": series, "IndexNumber": 1, "IsFolder": true},
			ep(1),
		}
		if up == "a" {
			return append(items, ep(2))
		}
		return append(items, ep(3))
	}
	return map[string]library{
		"a": {
			views: []map[string]any{{"Id": hexID("va"), "Name": "Movies", "CollectionType": "movies"},
				{"Id": hexID("vsa"), "Name": "Shows", "CollectionType": "tvshows"}},
			items: map[string][]map[string]any{
				hexID("va"):  {movie(shared["a"], "Shared", "1", "Comedy", hexID("va")), movie(aOnly, "Alpha", "2", "Drama", hexID("va"))},
				hexID("vsa"): show("a"),
			},
		},
		"b": {
			views: []map[string]any{{"Id": hexID("vb"), "Name": "Movies", "CollectionType": "movies"},
				{"Id": hexID("vsb"), "Name": "Shows", "CollectionType": "tvshows"},
				{"Id": hexID("vmb"), "Name": "Music", "CollectionType": "music"}},
			items: map[string][]map[string]any{
				hexID("vb"):  {movie(shared["b"], "Shared", "1", "Comedy", hexID("vb")), movie(bOnly, "Bravo", "3", "Western", hexID("vb"))},
				hexID("vsb"): show("b"),
				hexID("vmb"): {{"Id": hexID("ab1"), "Type": "Audio", "Name": "Tone", "ParentId": hexID("vmb"), "MediaType": "Audio", "Album": "Tones"}},
			},
		},
	}
}

// wrap turns a bare JSON array body into {"Items": [...]}.
func (r resp) wrap() resp {
	r.body = append(append([]byte(`{"Items":`), r.body...), '}')
	return r
}

func names(t *testing.T, r resp) []string {
	t.Helper()
	var out []string
	m := r.json(t)
	list, _ := m["Items"].([]any)
	for _, it := range list {
		out = append(out, it.(map[string]any)["Name"].(string))
	}
	slices.Sort(out)
	return out
}

// TestSetOperations drives each set operation through the client API.
func TestSetOperations(t *testing.T) {
	for _, tc := range []struct {
		name      string
		mode      identity.Mode
		libraries []string
		movies    []string
		episodes  []string
		genres    []string
		music     []string
		sources   map[string][]string // movie -> upstreams PlaybackInfo asks
		seriesUp  string              // upstream serving the series' artwork
	}{
		{"union", identity.Mode{Op: identity.Union},
			[]string{"Movies", "Music", "Shows"}, []string{"Alpha", "Bravo", "Shared"}, []string{"Episode 1", "Episode 2", "Episode 3"},
			[]string{"Comedy", "Drama", "Western"}, []string{"Tone"}, map[string][]string{"Shared": {"a", "b"}}, "a"},
		{"intersection", identity.Mode{Op: identity.Intersection},
			[]string{"Movies", "Shows"}, []string{"Shared"}, []string{"Episode 1"},
			[]string{"Comedy"}, nil, map[string][]string{"Shared": {"a", "b"}}, "a"},
		{"a minus b", identity.Mode{Op: identity.Difference, Base: "a", Subtract: []string{"b"}},
			[]string{"Movies", "Shows"}, []string{"Alpha"}, []string{"Episode 2"},
			[]string{"Drama"}, nil, map[string][]string{"Alpha": {"a"}}, "a"},
		{"b minus a", identity.Mode{Op: identity.Difference, Base: "b", Subtract: []string{"a"}},
			[]string{"Movies", "Music", "Shows"}, []string{"Bravo"}, []string{"Episode 3"},
			[]string{"Western"}, []string{"Tone"}, map[string][]string{"Bravo": {"b"}}, "b"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnvWith(t, tc.mode, setData())
			for _, f := range []*fakeUpstream{e.a, e.b} {
				f.media = map[string][]map[string]any{}
				for _, id := range []string{shared[f.name], aOnly, bOnly} {
					f.media[id] = []map[string]any{mediaSource(id, "h264", 1920, 1080, 100)}
				}
			}
			e.login()

			views := e.do("GET", "/UserViews?userId="+e.userID, e.token, nil)
			if got := names(t, views); !slices.Equal(got, tc.libraries) {
				t.Errorf("libraries = %v, want %v", got, tc.libraries)
			}
			libs := map[string]string{}
			for _, v := range views.json(t)["Items"].([]any) {
				libs[v.(map[string]any)["Name"].(string)] = v.(map[string]any)["Id"].(string)
			}

			q := "/Users/" + e.userID + "/Items?Recursive=true&IncludeItemTypes=Movie"
			if got := names(t, e.do("GET", q, e.token, nil)); !slices.Equal(got, tc.movies) {
				t.Errorf("movies = %v, want %v", got, tc.movies)
			}
			if got := names(t, e.do("GET", "/Users/"+e.userID+"/Items/Latest?ParentId="+libs["Movies"], e.token, nil).wrap()); !slices.Equal(got, tc.movies) {
				t.Errorf("latest movies = %v, want %v", got, tc.movies)
			}
			if got := names(t, e.do("GET", "/Users/"+e.userID+"/Items?Recursive=true&IncludeItemTypes=Episode", e.token, nil)); !slices.Equal(got, tc.episodes) {
				t.Errorf("episodes = %v, want %v", got, tc.episodes)
			}
			if got := names(t, e.do("GET", "/Genres?userId="+e.userID, e.token, nil)); !slices.Equal(got, tc.genres) {
				t.Errorf("genres = %v, want %v", got, tc.genres)
			}
			if got := names(t, e.do("GET", "/Users/"+e.userID+"/Items?Recursive=true&IncludeItemTypes=Audio", e.token, nil)); !slices.Equal(got, tc.music) {
				t.Errorf("music = %v, want %v", got, tc.music)
			}

			// The series counts only visible episodes, and its artwork comes
			// from a source copy.
			series := e.uid("a", hexID("5ea"))
			s := e.do("GET", "/Users/"+e.userID+"/Items/"+series, e.token, nil).json(t)
			if s["RecursiveItemCount"] != float64(len(tc.episodes)) {
				t.Errorf("series RecursiveItemCount = %v, want %d", s["RecursiveItemCount"], len(tc.episodes))
			}
			if img := e.do("GET", "/Items/"+series+"/Images/Primary", "", nil).json(t); img["Upstream"] != tc.seriesUp {
				t.Errorf("series artwork from %v, want %s", img["Upstream"], tc.seriesUp)
			}

			// Every movie is reachable by ID exactly when it is visible.
			ids := map[string]string{"Alpha": e.uid("a", aOnly), "Bravo": e.uid("b", bOnly), "Shared": e.uid("a", shared["a"])}
			for name, id := range ids {
				want := slices.Contains(tc.movies, name)
				r := e.do("GET", "/Users/"+e.userID+"/Items/"+id, e.token, nil)
				if (r.code == 200) != want {
					t.Errorf("%s by ID = %d, visible %v", name, r.code, want)
				}
				if r := e.do("POST", "/Items/"+id+"/PlaybackInfo", e.token, map[string]any{}); (r.code == 200) != want {
					t.Errorf("%s PlaybackInfo = %d, visible %v", name, r.code, want)
				}
				if !want {
					if r := e.do("POST", "/UserFavoriteItems/"+id, e.token, nil); r.code != 404 {
						t.Errorf("favoriting hidden %s = %d", name, r.code)
					}
				}
				if r := e.do("GET", "/Search/Hints?searchTerm="+url.QueryEscape(name)+"&userId="+e.userID, e.token, nil); r.code == 200 {
					found := strings.Contains(string(r.body), `"Name":"`+name+`"`)
					if found != want {
						t.Errorf("search for %s found %v, visible %v", name, found, want)
					}
				}
			}
			// PlaybackInfo asks only the upstreams whose copies take part.
			real := map[string]map[string]string{"Alpha": {"a": aOnly}, "Bravo": {"b": bOnly}, "Shared": shared}
			for name, ups := range tc.sources {
				for _, f := range []*fakeUpstream{e.a, e.b} {
					asked := real[name][f.name] != "" && len(f.find("/Items/"+real[name][f.name]+"/PlaybackInfo")) > 0
					if asked != slices.Contains(ups, f.name) {
						t.Errorf("PlaybackInfo for %s asked %s: %v", name, f.name, asked)
					}
				}
			}
		})
	}
}
