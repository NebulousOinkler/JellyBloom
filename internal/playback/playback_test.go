package playback

import (
	"encoding/json"
	"testing"
)

func ms(js string) map[string]any {
	m, err := decode([]byte(js))
	if err != nil {
		panic(err)
	}
	return m
}

func TestLabel(t *testing.T) {
	for js, want := range map[string]string{
		`{"MediaStreams":[{"Type":"Video","Codec":"hevc","Height":2160,"VideoRange":"HDR"}]}`:    "4K HEVC HDR",
		`{"MediaStreams":[{"Type":"Video","Codec":"h264","Height":720,"VideoRange":"SDR"}]}`:     "720p H264",
		`{"Container":"flac","Bitrate":900000,"MediaStreams":[{"Type":"Audio","Codec":"flac"}]}`: "FLAC 900kbps",
	} {
		if got := label(ms(js)); got != want {
			t.Errorf("label(%s) = %q, want %q", js, got, want)
		}
	}
}

func TestVersionKey(t *testing.T) {
	a := ms(`{"Container":"mkv","Size":100,"RunTimeTicks":72000000000,"MediaStreams":[{"Type":"Video","Codec":"h264","Width":1920,"Height":1080},{"Type":"Audio","Codec":"aac","Channels":2}]}`)
	same := ms(`{"Container":"MKV","Size":100,"RunTimeTicks":72000400000,"Bitrate":5,"MediaStreams":[{"Type":"Video","Codec":"H264","Width":1920,"Height":1080},{"Type":"Audio","Codec":"AAC","Channels":2}]}`)
	other := ms(`{"Container":"mkv","Size":101,"RunTimeTicks":72000000000,"MediaStreams":[{"Type":"Video","Codec":"h264","Width":1920,"Height":1080},{"Type":"Audio","Codec":"aac","Channels":2}]}`)
	if versionKey(a) != versionKey(same) {
		t.Errorf("identical files must share a key:\n%s\n%s", versionKey(a), versionKey(same))
	}
	if versionKey(a) == versionKey(other) {
		t.Error("different sizes must not share a key")
	}
}

func TestRankVersions(t *testing.T) {
	mk := func(js string) *version { return &version{key: js, cands: []candidate{{ms: ms(js)}}} }
	transcodeOnly4K := mk(`{"SupportsTranscoding":true,"MediaStreams":[{"Type":"Video","Height":2160}]}`)
	direct720 := mk(`{"SupportsDirectPlay":true,"MediaStreams":[{"Type":"Video","Height":720}]}`)
	direct1080 := mk(`{"SupportsDirectPlay":true,"MediaStreams":[{"Type":"Video","Height":1080}]}`)
	vs := []*version{transcodeOnly4K, direct720, direct1080}
	rankVersions(vs)
	if vs[0] != direct1080 || vs[1] != direct720 || vs[2] != transcodeOnly4K {
		b, _ := json.Marshal([]string{vs[0].key, vs[1].key, vs[2].key})
		t.Errorf("order = %s", b)
	}
}
