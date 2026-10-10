#!/bin/sh
# Generates synthetic test media with Jellyfin's bundled ffmpeg (nothing is
# downloaded). Server A gets video only; server B gets music plus one movie
# that is byte-identical to one on A.
set -eu
FF=${FFMPEG:-/Applications/Jellyfin.app/Contents/MacOS/ffmpeg}
D=$(cd "$(dirname "$0")" && pwd)/data/media

poster() { # out pattern
	[ -f "$1" ] || "$FF" -nostdin -loglevel error -y -f lavfi -i "$2=size=400x600" -frames:v 1 "$1"
}
posters() { # local artwork: Jellyfin always reads poster.jpg/cover.jpg next to media
	for dir in "$D"/*/movies/*/ "$D"/a/shows/*/; do poster "$dir/poster.jpg" testsrc2; done
	for dir in "$D"/b/music/*/*/; do poster "$dir/cover.jpg" smptebars; done
}
[ -f "$D/.done" ] && { posters; echo "media already generated (posters ensured)"; exit 0; }

video() { # out seconds size pattern
	mkdir -p "$(dirname "$1")"
	"$FF" -nostdin -loglevel error -y -f lavfi -i "$4=size=$3:rate=24:duration=$2" \
		-f lavfi -i "sine=frequency=440:duration=$2" -c:v libx264 -preset ultrafast -pix_fmt yuv420p \
		-c:a aac -b:a 96k -shortest "$1"
}
track() { # out seconds freq artist album title n year
	mkdir -p "$(dirname "$1")"
	"$FF" -nostdin -loglevel error -y -f lavfi -i "sine=frequency=$3:duration=$2" -c:a flac \
		-metadata artist="$4" -metadata album_artist="$4" -metadata album="$5" -metadata title="$6" \
		-metadata track="$7" -metadata date="$8" "$1"
}
nfo() { # out title year tmdb
	cat > "$1" <<NFO
<?xml version="1.0" encoding="utf-8" standalone="yes"?>
<movie><title>$2</title><year>$3</year><tmdbid>$4</tmdbid><uniqueid type="tmdb" default="true">$4</uniqueid></movie>
NFO
}

shared="Night of the Test Pattern (1968)"
video "$D/a/movies/$shared/$shared.mkv" 30 1280x720 testsrc2
video "$D/a/movies/$shared/$shared - 480p.mkv" 30 854x480 testsrc2
nfo "$D/a/movies/$shared/$shared.nfo" "Night of the Test Pattern" 1968 999001
video "$D/a/movies/Colour Bars (1971)/Colour Bars (1971).mkv" 20 1280x720 smptebars
video "$D/a/shows/Test Show/Season 01/Test Show S01E01.mkv" 15 1280x720 testsrc
video "$D/a/shows/Test Show/Season 01/Test Show S01E02.mkv" 15 1280x720 rgbtestsrc

mkdir -p "$D/b/movies/$shared"
cp "$D/a/movies/$shared/$shared.mkv" "$D/a/movies/$shared/$shared.nfo" "$D/b/movies/$shared/"
track "$D/b/music/Sine Wave Ensemble/Pure Tones (2001)/01 - A440.flac" 20 440 "Sine Wave Ensemble" "Pure Tones" "A440" 1 2001
track "$D/b/music/Sine Wave Ensemble/Pure Tones (2001)/02 - C523.flac" 20 523 "Sine Wave Ensemble" "Pure Tones" "C523" 2 2001
posters
touch "$D/.done"
echo "media generated in $D"
