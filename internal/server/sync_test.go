package server

import (
	"encoding/json"
	"testing"

	"github.com/NebulousOinkler/JellyBloom/internal/identity"
)

// TestSyncBetweenInstances moves user state between two JellyBloom
// instances through the admin endpoints, in both directions, as
// `jellybloom sync` does.
func TestSyncBetweenInstances(t *testing.T) {
	home, laptop := newEnv(t, identity.Mode{}), newEnv(t, identity.Mode{})
	for _, e := range []*env{home, laptop} {
		e.login()
		if r := e.do("GET", "/JellyBloom/UserData", e.token, nil); r.code != 403 {
			t.Errorf("non-admin export = %d", r.code)
		}
		if r := e.do("POST", "/JellyBloom/UserData", e.token, map[string]any{"Version": 1}); r.code != 403 {
			t.Errorf("non-admin merge = %d", r.code)
		}
		e.loginAs("boss")
	}
	movie := home.uid("a", shared["a"])
	if movie != laptop.uid("a", shared["a"]) {
		t.Fatal("the two instances disagree on the shared movie's ID")
	}
	// Watched on the trip; favorited at home; a playlist made on the trip.
	laptop.do("POST", "/UserPlayedItems/"+movie, laptop.token, nil)
	home.do("POST", "/UserFavoriteItems/"+home.uid("b", bOnly), home.token, nil)
	pl := laptop.do("POST", "/Playlists", laptop.token, map[string]any{"Name": "Trip", "Ids": []string{movie}, "UserId": laptop.userID}).json(t)

	export := func(e *env) any {
		var v any
		if err := json.Unmarshal(e.do("GET", "/JellyBloom/UserData", e.token, nil).body, &v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	fromHome, fromLaptop := export(home), export(laptop)
	if r := home.do("POST", "/JellyBloom/UserData", home.token, fromLaptop); r.code != 200 {
		t.Fatalf("merge into home = %d %s", r.code, r.body)
	}
	if r := laptop.do("POST", "/JellyBloom/UserData", laptop.token, fromHome); r.code != 200 {
		t.Fatalf("merge into laptop = %d %s", r.code, r.body)
	}

	userData := func(e *env, id string) map[string]any {
		return e.do("GET", "/Users/"+e.userID+"/Items/"+id, e.token, nil).json(t)["UserData"].(map[string]any)
	}
	if ud := userData(home, movie); ud["Played"] != true {
		t.Errorf("home after sync: %v", ud)
	}
	if ud := userData(laptop, laptop.uid("b", bOnly)); ud["IsFavorite"] != true {
		t.Errorf("laptop after sync: %v", ud)
	}
	if r := home.do("GET", "/Playlists/"+pl["Id"].(string)+"/Items?userId="+home.userID, home.token, nil); r.code != 200 || len(names(t, r)) != 1 {
		t.Errorf("playlist at home = %d %s", r.code, r.body)
	}
}
