package rewrite

import (
	"encoding/json"
	"strings"
	"testing"
)

const (
	realA = "0123456789abcdef0123456789abcdef"
	uniA  = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	tag   = "fedcba9876543210fedcba9876543210"
	upTok = "11111111111111111111111111111111"
	myTok = "22222222222222222222222222222222"
)

func replacer() *Replacer {
	return &Replacer{
		GUID: func(n string) (string, bool) {
			if n == realA {
				return uniA, true
			}
			return "", false
		},
		Literals: []string{"http://up:8096", "https://me.example", upTok, myTok},
	}
}

func TestReplaceGUIDs(t *testing.T) {
	r := replacer()
	cases := map[string]string{
		"/Items/" + realA + "/Images":                          "/Items/" + uniA + "/Images",
		"/Items/" + strings.ToUpper(realA):                     "/Items/" + uniA,
		"id=01234567-89ab-cdef-0123-456789abcdef&x=1":          "id=aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa&x=1",
		"hash " + realA + realA:                                "hash " + realA + realA, // 64-hex run: not a GUID
		"x" + realA:                                            "x" + realA,             // glued to a word char
		"http://up:8096/Videos/" + realA + "?api_key=" + upTok: "https://me.example/Videos/" + uniA + "?api_key=" + myTok,
	}
	for in, want := range cases {
		if got := r.String(in); got != want {
			t.Errorf("String(%q)\n got %q\nwant %q", in, got, want)
		}
	}
}

func TestJSONSkipsHashKeys(t *testing.T) {
	in := `{"Id":"` + realA + `","ImageTags":{"Primary":"` + realA + `"},"PrimaryImageTag":"` + realA + `",` +
		`"BlurHashes":{"Primary":{"` + realA + `":"LEHV6n"}},"Etag":"` + realA + `","ParentId":"` + realA + `",` +
		`"RunTimeTicks":12345678901234567,"Name":"<Tom & Jerry>","List":["` + realA + `"]}`
	var got map[string]any
	if err := json.Unmarshal(replacer().JSON([]byte(in)), &got); err != nil {
		t.Fatal(err)
	}
	if got["Id"] != uniA || got["ParentId"] != uniA || got["List"].([]any)[0] != uniA {
		t.Errorf("IDs not rewritten: %v", got)
	}
	if got["ImageTags"].(map[string]any)["Primary"] != realA || got["PrimaryImageTag"] != realA || got["Etag"] != realA {
		t.Errorf("hash keys must not be rewritten: %v", got)
	}
	if _, ok := got["BlurHashes"].(map[string]any)["Primary"].(map[string]any)[realA]; !ok {
		t.Errorf("BlurHashes keys must not be rewritten: %v", got["BlurHashes"])
	}
	if got["Name"] != "<Tom & Jerry>" {
		t.Errorf("Name = %v", got["Name"])
	}
	out := string(replacer().JSON([]byte(in)))
	if !strings.Contains(out, "12345678901234567") || strings.Contains(out, "\\u003c") {
		t.Errorf("numbers or HTML escaping changed: %s", out)
	}
}

func TestJSONFallsBackToText(t *testing.T) {
	if got := string(replacer().JSON([]byte("not json " + realA))); got != "not json "+uniA {
		t.Errorf("got %q", got)
	}
}

func TestEachGUID(t *testing.T) {
	var got []string
	EachGUID("/Users/01234567-89AB-cdef-0123-456789abcdef/Items/"+tag+"?x="+realA+realA, func(n string) { got = append(got, n) })
	if len(got) != 2 || got[0] != realA || got[1] != tag {
		t.Errorf("got %v", got)
	}
}

func TestIsText(t *testing.T) {
	for ct, want := range map[string]bool{
		"application/json; charset=utf-8": true, "application/vnd.apple.mpegurl": true, "application/x-mpegURL": true,
		"text/html": true, "video/mp4": false, "image/jpeg": false, "application/octet-stream": false,
	} {
		if IsText(ct) != want {
			t.Errorf("IsText(%q) = %v", ct, !want)
		}
	}
}

func TestTemplate(t *testing.T) {
	if got := Template("/Items/" + realA + "/Images/Primary?tag=" + tag); got != "/Items/{id}/Images/Primary?tag={id}" {
		t.Errorf("got %q", got)
	}
}
