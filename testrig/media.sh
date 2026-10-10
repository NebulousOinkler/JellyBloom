#!/bin/sh
# Generates synthetic test media with Jellyfin's bundled ffmpeg (nothing is
# downloaded). Server A gets movies and shows; server B gets music, an
# audiobook, movies byte-identical to two of A's, and broken copies of two
# more. Idempotent: existing files are kept, missing ones are generated.
set -eu
FF=${FFMPEG:-/Applications/Jellyfin.app/Contents/MacOS/ffmpeg}
D=$(cd "$(dirname "$0")" && pwd)/data/media

poster() { # out pattern
	[ -f "$1" ] || "$FF" -nostdin -loglevel error -y -f lavfi -i "$2=size=400x600" -frames:v 1 "$1"
}
video() { # out seconds size pattern
	[ -f "$1" ] && return
	mkdir -p "$(dirname "$1")"
	"$FF" -nostdin -loglevel error -y -f lavfi -i "$4=size=$3:rate=24:duration=$2" \
		-f lavfi -i "sine=frequency=440:duration=$2" -c:v libx264 -preset ultrafast -pix_fmt yuv420p \
		-c:a aac -b:a 96k -shortest "$1"
}
video_cbr() { # out seconds size pattern kbps: constant bitrate, so the file has a predictable size
	[ -f "$1" ] && return
	mkdir -p "$(dirname "$1")"
	"$FF" -nostdin -loglevel error -y -f lavfi -i "$4=size=$3:rate=24:duration=$2" \
		-f lavfi -i "sine=frequency=440:duration=$2" -c:v libx264 -preset ultrafast -pix_fmt yuv420p \
		-b:v "$5k" -minrate "$5k" -maxrate "$5k" -bufsize "$(($5 * 2))k" -x264-params nal-hrd=cbr \
		-c:a aac -b:a 96k -shortest "$1"
}
track() { # out seconds freq artist album title n year
	[ -f "$1" ] && return
	mkdir -p "$(dirname "$1")"
	"$FF" -nostdin -loglevel error -y -f lavfi -i "sine=frequency=$3:duration=$2" -c:a flac \
		-metadata artist="$4" -metadata album_artist="$4" -metadata album="$5" -metadata title="$6" \
		-metadata track="$7" -metadata date="$8" "$1"
}
book() { # out seconds author title year
	[ -f "$1" ] && return
	mkdir -p "$(dirname "$1")"
	"$FF" -nostdin -loglevel error -y -f lavfi -i "sine=frequency=330:duration=$2" -ac 1 -c:a aac -b:a 32k \
		-metadata artist="$3" -metadata album_artist="$3" -metadata album="$4" -metadata title="$4" \
		-metadata date="$5" "$1"
}
nfo() { # out title year tmdb
	[ -f "$1" ] && return
	cat > "$1" <<NFO
<?xml version="1.0" encoding="utf-8" standalone="yes"?>
<movie><title>$2</title><year>$3</year><tmdbid>$4</tmdbid><uniqueid type="tmdb" default="true">$4</uniqueid></movie>
NFO
}
share() { # dir: copy A's movie folder to B byte-for-byte
	mkdir -p "$D/b/movies/$1"
	for f in "$D/a/movies/$1"/*.mkv "$D/a/movies/$1"/*.nfo; do
		case $f in *" - "*) continue ;; esac # A's extra versions stay on A
		[ -f "$D/b/movies/$1/$(basename "$f")" ] || cp "$f" "$D/b/movies/$1/"
	done
}

shared="Night of the Test Pattern (1968)"
video "$D/a/movies/$shared/$shared.mkv" 30 1280x720 testsrc2
video "$D/a/movies/$shared/$shared - 480p.mkv" 30 854x480 testsrc2
nfo "$D/a/movies/$shared/$shared.nfo" "Night of the Test Pattern" 1968 999001
share "$shared"
video "$D/a/movies/Colour Bars (1971)/Colour Bars (1971).mkv" 20 1280x720 smptebars

# Long enough for Jellyfin's resume rules (>= 300 s) and for mid-stream
# failover between the two identical copies.
long="Long Test Pattern (2001)"
video "$D/a/movies/$long/$long.mkv" 600 640x360 testsrc2
nfo "$D/a/movies/$long/$long.nfo" "Long Test Pattern" 2001 999002
share "$long"

# Broken copies, for routing around broken files: B's "Damaged Pattern" has
# the same size but 1 MiB zeroed mid-file (bit rot a first/last-MB hash
# misses); B's "Truncated Pattern" is cut at 60% (its header still claims the
# full runtime).
damaged="Damaged Pattern (2005)"
video_cbr "$D/a/movies/$damaged/$damaged.mkv" 120 640x360 smptehdbars 4000
nfo "$D/a/movies/$damaged/$damaged.nfo" "Damaged Pattern" 2005 999003
share "$damaged"
f="$D/b/movies/$damaged/$damaged.mkv"
if [ ! -f "$D/b/movies/$damaged/.damaged" ]; then
	dd if=/dev/zero of="$f" bs=1048576 seek=$(($(stat -f%z "$f") / 2 / 1048576)) count=1 conv=notrunc 2>/dev/null
	touch "$D/b/movies/$damaged/.damaged"
fi
truncated="Truncated Pattern (2006)"
video "$D/a/movies/$truncated/$truncated.mkv" 120 640x360 pal75bars
nfo "$D/a/movies/$truncated/$truncated.nfo" "Truncated Pattern" 2006 999004
share "$truncated"
f="$D/b/movies/$truncated/$truncated.mkv"
if [ ! -f "$D/b/movies/$truncated/.truncated" ]; then
	head -c $(($(stat -f%z "$f") * 6 / 10)) "$f" > "$f.part" && mv "$f.part" "$f"
	touch "$D/b/movies/$truncated/.truncated"
fi

show="$D/a/shows/Test Show"
video "$show/Season 01/Test Show S01E01.mkv" 15 1280x720 testsrc
video "$show/Season 01/Test Show S01E02.mkv" 15 1280x720 rgbtestsrc
video "$show/Season 01/Test Show S01E03.mkv" 15 1280x720 smptehdbars
video "$show/Season 01/Test Show S01E04.mkv" 15 1280x720 yuvtestsrc
video "$show/Season 02/Test Show S02E01.mkv" 15 1280x720 testsrc2

music="$D/b/music/Sine Wave Ensemble"
track "$music/Pure Tones (2001)/01 - A440.flac" 20 440 "Sine Wave Ensemble" "Pure Tones" "A440" 1 2001
track "$music/Pure Tones (2001)/02 - C523.flac" 20 523 "Sine Wave Ensemble" "Pure Tones" "C523" 2 2001
track "$music/Long Tones (2003)/01 - Drone.flac" 360 110 "Sine Wave Ensemble" "Long Tones" "Drone" 1 2003

book "$D/b/books/Sine Wave Reader/The Long Tone (2005)/The Long Tone.m4a" 720 "Sine Wave Reader" "The Long Tone" 2005

# Local artwork: Jellyfin always reads poster.jpg/cover.jpg next to media.
for dir in "$D"/*/movies/*/ "$D"/a/shows/*/; do poster "$dir/poster.jpg" testsrc2; done
for dir in "$D"/b/music/*/*/ "$D"/b/books/*/*/; do poster "$dir/cover.jpg" smptebars; done
rm -f "$D/.done" # marker from older versions of this script
echo "media ready in $D"
