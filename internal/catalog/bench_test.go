package catalog

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/NebulousOinkler/JellyBloom/internal/identity"
	"github.com/NebulousOinkler/JellyBloom/internal/index"
)

// benchCatalog indexes 25k movies on each of two upstreams (half overlapping),
// each with genres, people, and a sizable DTO.
func benchCatalog(b *testing.B) (*Catalog, string) {
	idx, err := index.Open(filepath.Join(b.TempDir(), "db"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { idx.Close() })
	for _, up := range []string{"a", "b"} {
		off := map[string]int{"a": 0, "b": 12500}[up]
		view := g(up + "0")
		var items []dto
		for i := range 25000 {
			n := i + off
			items = append(items, dto{"Id": g(fmt.Sprintf("%s%x", up, i+1)), "Type": "Movie", "Name": fmt.Sprintf("Movie %d", n),
				"ProductionYear": 1950 + n%70, "ParentId": view, "DateCreated": fmt.Sprintf("2020-01-01T00:%02d:00Z", n%60),
				"ProviderIds": dto{"Tmdb": fmt.Sprint(n)}, "Overview": "A long overview of the film that takes up some space.",
				"GenreItems": []dto{{"Name": fmt.Sprintf("Genre %d", n%20), "Id": g(fmt.Sprintf("%sc%x", up, n%20))}},
				"People":     []dto{{"Name": fmt.Sprintf("Actor %d", n%500), "Id": g(fmt.Sprintf("%sd%x", up, n%500)), "Type": "Actor"}}})
		}
		put(b, idx, up, []dto{{"Id": view, "Name": "Movies", "CollectionType": "movies"}}, map[string][]dto{view: items})
	}
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	res, _ := identity.New(idx, identity.Mode{Op: identity.Union, Upstreams: []string{"a", "b"}}, quiet)
	if _, err := res.Rebuild(context.Background()); err != nil {
		b.Fatal(err)
	}
	cat := New(Options{DB: idx.DB(), Resolver: res, Upstreams: []string{"a", "b"}, ServerID: "s", Logger: quiet})
	if err := cat.Reload(context.Background()); err != nil {
		b.Fatal(err)
	}
	return cat, cat.snap.Load().views[0].id
}

func BenchmarkReload(b *testing.B) {
	cat, _ := benchCatalog(b)
	b.ResetTimer()
	for range b.N {
		if err := cat.Reload(context.Background()); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkQueryPage(b *testing.B) {
	cat, view := benchCatalog(b)
	q := Query{ParentID: view, Recursive: true, IncludeTypes: []string{"Movie"}, SortBy: []string{"SortName"}, StartIndex: 1000, Limit: 100}
	b.ResetTimer()
	for range b.N {
		r, err := cat.Query(context.Background(), q, DTOOptions{Fields: []string{"Overview", "Genres"}})
		if err != nil || len(r.Items) != 100 {
			b.Fatal(err)
		}
	}
}
