package server

import (
	"bytes"
	"io/fs"
	"path/filepath"
	"testing"

	"github.com/NebulousOinkler/JellyBloom/internal/identity"
)

// TestNoLocalStorage plays media through JellyBloom (relays with failover,
// HLS, artwork, user data, a playlist), then checks that nothing but the
// database was written, and that the database did not take in media: the
// pure-proxy invariant. (internal/storagecheck checks the source.)
func TestNoLocalStorage(t *testing.T) {
	// The test's temporary root holds the database, and TMPDIR points into
	// it too, so anything the process writes lands under it.
	watch := filepath.Dir(t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())

	data := testFile(4 << 20)
	e := newEnvWith(t, identity.Mode{}, fileData(len(data), len(data)))
	e.withFiles(data, data)
	e.login()
	item := e.uid("a", shared["a"])
	relayed := 0
	for i := range 3 {
		info := e.do("POST", "/Items/"+item+"/PlaybackInfo", e.token, map[string]any{}).json(t)
		ms := info["MediaSources"].([]any)[0].(map[string]any)["Id"].(string)
		if i == 1 { // the second play breaks off on a and continues on b
			e.a.mu.Lock()
			e.a.cut = 1 << 20
			e.a.mu.Unlock()
		}
		r := e.do("GET", "/Videos/"+item+"/stream?Static=true&MediaSourceId="+ms+"&api_key="+e.token, "", nil)
		if !bytes.Equal(r.body, data) {
			t.Fatalf("play %d: %d bytes", i, len(r.body))
		}
		relayed += len(r.body)
		e.do("GET", "/videos/"+item+"/hls1/main/0.ts?MediaSourceId="+ms+"&PlaySessionId="+info["PlaySessionId"].(string)+"&api_key="+e.token, "", nil)
	}
	e.do("GET", "/Items/"+item+"/Images/Primary", "", nil)
	e.do("POST", "/UserFavoriteItems/"+item, e.token, nil)
	e.do("POST", "/Playlists", e.token, map[string]any{"Name": "P", "Ids": []string{item}, "UserId": e.userID})

	var dbBytes int64
	err := filepath.WalkDir(watch, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		info, _ := d.Info()
		switch filepath.Base(path) {
		case "db", "db-wal", "db-shm":
			dbBytes += info.Size()
		default:
			rel, _ := filepath.Rel(watch, path)
			t.Errorf("unexpected file %s (%d bytes)", rel, info.Size())
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// Metadata, fingerprints, and block hashes only: far less than the media.
	if dbBytes > int64(relayed)/4 {
		t.Errorf("database is %d bytes after relaying %d: is media being stored?", dbBytes, relayed)
	}
	t.Logf("relayed %d bytes; database %d bytes", relayed, dbBytes)
}
