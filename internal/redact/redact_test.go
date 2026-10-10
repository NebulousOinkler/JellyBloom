package redact

import (
	"bytes"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"testing"
)

func TestString(t *testing.T) {
	for in, want := range map[string]string{
		`Get "http://up:8096/videos/x/master.m3u8?DeviceId=d&ApiKey=abc123&x=1": dial tcp: refused`: `Get "http://up:8096/videos/x/master.m3u8?DeviceId=d&ApiKey=[REDACTED]&x=1": dial tcp: refused`,
		`/socket?api_key=abc123&deviceId=d`:                     `/socket?api_key=[REDACTED]&deviceId=d`,
		`MediaBrowser Client="x", DeviceId="d", Token="abc123"`: `MediaBrowser Client="x", DeviceId="d", Token="[REDACTED]"`,
		`{"AccessToken":"abc123","User":{"Id":"u"}}`:            `{"AccessToken":"[REDACTED]","User":{"Id":"u"}}`,
		`{"Username":"me","Pw":"hunter2"}`:                      `{"Username":"me","Pw":"[REDACTED]"}`,
		`{"Secret":"qc-secret"}`:                                `{"Secret":"[REDACTED]"}`,
		`upstream a: stored credential was rejected; run x`:     `upstream a: stored credential was rejected; run x`,
	} {
		if got := String(in); got != want {
			t.Errorf("String(%q)\n got %q\nwant %q", in, got, want)
		}
	}
}

func TestReplaceAttrCatchesTransportErrors(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{ReplaceAttr: ReplaceAttr}))
	// A real transport error: Go puts the full request URL in it.
	_, err := http.Get("http://127.0.0.1:1/videos/x/main.m3u8?ApiKey=supersecret")
	if err == nil {
		t.Skip("port 1 unexpectedly open")
	}
	log.Warn("upstream request failed", "err", err, "note", "Token=\"supersecret\"")
	log.Info("plain", "e", errors.New("pw: supersecret"))
	if strings.Contains(buf.String(), "supersecret") {
		t.Errorf("secret reached the log:\n%s", buf.String())
	}
}
