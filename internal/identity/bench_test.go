package identity

import (
	"context"
	"fmt"
	"testing"
)

// BenchmarkRebuild measures a rebuild over two upstreams of 25k items each
// (half overlapping): movies, plus series with seasons and episodes.
func BenchmarkRebuild(b *testing.B) {
	f := newFixture(b)
	for _, up := range []string{"a", "b"} {
		var items []dto
		off := 0
		if up == "b" {
			off = 5000
		}
		for i := range 10000 {
			n := i + off
			items = append(items, movie(fmt.Sprintf("%s-m%d", up, i), fmt.Sprintf("Movie %d", n), 1990+n%30, map[string]string{"Tmdb": fmt.Sprint(n)}))
		}
		for s := range 100 {
			sid := fmt.Sprintf("%s-s%d", up, s)
			items = append(items, dto{"Id": sid, "Type": "Series", "Name": fmt.Sprint("Show ", s), "ProviderIds": ids(map[string]string{"Tvdb": fmt.Sprint(s)})})
			for se := range 3 {
				seid := fmt.Sprintf("%s-%d", sid, se)
				items = append(items, dto{"Id": seid, "Type": "Season", "SeriesId": sid, "ParentId": sid, "IndexNumber": se})
				for e := range 49 {
					items = append(items, episode(fmt.Sprintf("%s-%d", seid, e), sid, seid, se, e))
				}
			}
		}
		f.set(up, items...)
	}
	r, _ := New(f.idx, Mode{Op: Union, Upstreams: []string{"a", "b"}}, discardLogger())
	b.ResetTimer()
	for range b.N {
		if _, err := r.Rebuild(context.Background()); err != nil {
			b.Fatal(err)
		}
	}
}
