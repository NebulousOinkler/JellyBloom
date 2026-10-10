package proxy

import "testing"

func TestPolicy(t *testing.T) {
	p, err := NewPolicy([]string{`^/introskipper/`})
	if err != nil {
		t.Fatal(err)
	}
	id := "0123456789abcdef0123456789abcdef"
	cases := []struct {
		method, path string
		want         Decision
	}{
		{"GET", "/Items/" + id, Authed},
		{"GET", "/Items/" + id + "/Images/Primary", Anonymous},
		{"GET", "/Localization/Cultures", Anonymous},
		{"GET", "/System/Logs", Deny},
		{"GET", "/system", Deny},
		{"GET", "/SystemFoo", Authed},
		{"GET", "/Auth/Keys", Deny},
		{"GET", "/Users", Deny},
		{"GET", "/Users/" + id + "/Items", Authed},
		{"GET", "/Plugins", Deny},
		{"POST", "/Library/Refresh", Deny},
		{"DELETE", "/Items/" + id, Deny},
		{"POST", "/Items/" + id, Deny},
		{"POST", "/Sessions/Playing/Progress", Authed},
		{"POST", "/Items/" + id + "/PlaybackInfo", Authed},
		{"DELETE", "/Videos/ActiveEncodings", Authed},
		{"POST", "/UserPlayedItems/" + id, Authed},
		{"DELETE", "/Users/" + id + "/FavoriteItems/" + id, Authed},
		{"POST", "/IntroSkipper/Foo", Authed},
		{"POST", "/OtherPlugin/Foo", Deny},
	}
	for _, c := range cases {
		if got := p.Decide(c.method, c.path); got != c.want {
			t.Errorf("%s %s = %v, want %v", c.method, c.path, got, c.want)
		}
	}
}
