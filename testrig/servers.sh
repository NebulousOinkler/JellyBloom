#!/bin/sh
# Starts or stops the two local Jellyfin test servers (localhost only) and,
# once testrig/setup.py has written its config, JellyBloom in front of them.
#   testrig/servers.sh start | stop | status | restart-jb
#   testrig/servers.sh stop-server a|b | start-server a|b   (simulate an upstream outage)
#   testrig/servers.sh faults a|b ['{"cut_after": 1000000}' | clear]   (inject faults; see testrig/faultproxy)
set -eu
JF=${JELLYFIN:-/Applications/Jellyfin.app/Contents/MacOS/jellyfin}
WEB=${JELLYFIN_WEB:-/Applications/Jellyfin.app/Contents/Resources/jellyfin-web}
ROOT=$(cd "$(dirname "$0")" && pwd)/data

network_xml() { # port
	cat <<XML
<?xml version="1.0" encoding="utf-8"?>
<NetworkConfiguration xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance" xmlns:xsd="http://www.w3.org/2001/XMLSchema">
  <InternalHttpPort>$1</InternalHttpPort>
  <PublicHttpPort>$1</PublicHttpPort>
  <EnableHttps>false</EnableHttps>
  <LocalNetworkAddresses><string>127.0.0.1</string></LocalNetworkAddresses>
  <AutoDiscovery>false</AutoDiscovery>
  <EnableUPnP>false</EnableUPnP>
  <EnableIPv6>false</EnableIPv6>
</NetworkConfiguration>
XML
}

start() { # name port
	d="$ROOT/jf-$1"
	mkdir -p "$d/config" "$d/data" "$d/cache" "$d/log"
	[ -f "$d/config/network.xml" ] || network_xml "$2" > "$d/config/network.xml"
	if [ -f "$d/pid" ] && kill -0 "$(cat "$d/pid")" 2>/dev/null; then
		echo "jf-$1 already running (pid $(cat "$d/pid"))"; return
	fi
	nohup "$JF" --datadir "$d/data" --configdir "$d/config" --cachedir "$d/cache" --logdir "$d/log" \
		--webdir "$WEB" --ffmpeg "$(dirname "$JF")/ffmpeg" --nonetchange > "$d/stdout.log" 2>&1 &
	echo $! > "$d/pid"
	echo "jf-$1 starting on http://127.0.0.1:$2 (pid $!)"
}

# JellyBloom reaches server a through :8211 and b through :8212.
start_faults() {
	d="$ROOT/faults"
	mkdir -p "$d"
	if [ -f "$d/pid" ] && kill -0 "$(cat "$d/pid")" 2>/dev/null; then
		echo "faultproxy already running (pid $(cat "$d/pid"))"; return
	fi
	(cd "$(dirname "$ROOT")/.." && go build -o "$ROOT/faultproxy" ./testrig/faultproxy)
	nohup "$ROOT/faultproxy" -listen 127.0.0.1:8211=http://127.0.0.1:8201 -listen 127.0.0.1:8212=http://127.0.0.1:8202 \
		> "$d/faultproxy.log" 2>&1 &
	echo $! > "$d/pid"
	echo "faultproxy on :8211 (a) and :8212 (b) (pid $!)"
}

start_jb() {
	d="$ROOT/jb"
	[ -f "$ROOT/jellybloom.yaml" ] || { echo "no JellyBloom config yet: run testrig/setup.py"; return; }
	mkdir -p "$d"
	(cd "$(dirname "$ROOT")/.." && go build -o "$ROOT/jellybloom" ./cmd/jellybloom)
	JELLYBLOOM_LOG=${JELLYBLOOM_LOG:-debug} nohup "$ROOT/jellybloom" -config "$ROOT/jellybloom.yaml" > "$d/jellybloom.log" 2>&1 &
	echo $! > "$d/pid"
	echo "JellyBloom starting on http://127.0.0.1:8200 (pid $!), log: $d/jellybloom.log"
}

stop() {
	for d in "$ROOT"/jf-* "$ROOT"/jb "$ROOT"/faults; do
		[ -f "$d/pid" ] || continue
		kill "$(cat "$d/pid")" 2>/dev/null && echo "stopped $(basename "$d")" || true
		rm -f "$d/pid"
	done
}

case "${1:-}" in
	start) start a 8201; start b 8202; start_faults; start_jb ;;
	stop) stop ;;
	restart-jb) kill "$(cat "$ROOT/jb/pid" 2>/dev/null)" 2>/dev/null || true; sleep 1; start_faults; start_jb ;;
	stop-server)
		d="$ROOT/jf-${2:?usage: $0 stop-server a|b}"
		[ -f "$d/pid" ] && kill "$(cat "$d/pid")" 2>/dev/null && echo "stopped jf-$2" || echo "jf-$2 not running"
		rm -f "$d/pid" ;;
	start-server)
		case "${2:-}" in a) start a 8201 ;; b) start b 8202 ;; *) echo "usage: $0 start-server a|b" >&2; exit 2 ;; esac ;;
	faults)
		case "${2:-}" in a) port=8211 ;; b) port=8212 ;; *) echo "usage: $0 faults a|b [JSON|clear]" >&2; exit 2 ;; esac
		case "${3:-}" in
			"") curl -s http://127.0.0.1:$port/__faults ;;
			clear) curl -s -X DELETE http://127.0.0.1:$port/__faults ;;
			*) curl -s -X POST -d "$3" http://127.0.0.1:$port/__faults ;;
		esac ;;
	status) for p in 8201 8202 8211 8212 8200; do printf '%s: ' $p; curl -s -m 2 http://127.0.0.1:$p/System/Info/Public | cut -c1-120 || echo down; echo; done ;;
	*) echo "usage: $0 start|stop|status|restart-jb|stop-server a|b|start-server a|b|faults a|b [JSON|clear]" >&2; exit 2 ;;
esac
