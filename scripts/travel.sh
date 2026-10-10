#!/bin/sh
# The traveling library: JellyBloom on a laptop, combining the laptop's own
# Jellyfin (a portable copy of part of your library) with your home server.
# See docs/TRAVELING-LIBRARY.md.
#
#   scripts/travel.sh init     write the configs and pair both servers (once)
#   scripts/travel.sh start    start the library and the packing/unpacking lists
#   scripts/travel.sh stop
#   scripts/travel.sh status
#   scripts/travel.sh sync     merge watch state and playlists with your home JellyBloom
#   scripts/travel.sh report   broken copies, and what exists on only one server
#
# Three JellyBloom instances run on the laptop:
#   library    union of laptop and home, laptop first: what you watch from
#   packing    home \ laptop: what is not packed yet
#   unpacking  laptop \ home: what to copy home when you are back
#
# Settings come from the environment on `init` and are saved in
# $TRAVEL_DIR/travel.env (default ~/.jellybloom-travel):
#   HOME_URL          home Jellyfin server, as the laptop reaches it (required)
#   HOME_NAME         its name; use the name your home JellyBloom gives it (default home)
#   LAPTOP_URL        the laptop's Jellyfin server (default http://127.0.0.1:8096)
#   LAPTOP_NAME       its name (default laptop)
#   USERS_FROM        your home JellyBloom's config: its users are copied, so the same
#                     people with the same passwords exist on both and can be synced
#                     (default: create one admin user now)
#   HOME_JELLYBLOOM   your home JellyBloom's URL, for `sync`
#   SYNC_USER         an admin user on both, for `sync` (default: the first admin user)
#   PAIRING_FROM      a JellyBloom data directory's secrets/ to copy pairing from,
#                     instead of pairing with Quick Connect
#   LIBRARY_LISTEN    where the library listens (default :8400, so TVs and phones on
#                     the same network can reach it)
#   PACKING_LISTEN    default 127.0.0.1:8401;  UNPACKING_LISTEN  default 127.0.0.1:8402
#   JELLYBLOOM        the jellybloom binary (default: on PATH, else built from this repo)
# `sync` reads the password from SYNC_PASSWORD_FILE if set, else prompts.
set -eu

TRAVEL_DIR=${TRAVEL_DIR:-$HOME/.jellybloom-travel}
ENV_FILE=$TRAVEL_DIR/travel.env
REPO=$(cd "$(dirname "$0")/.." && pwd)
INSTANCES="library packing unpacking"

die() { echo "travel: $*" >&2; exit 1; }

load() {
	[ -f "$ENV_FILE" ] || die "not set up yet: run scripts/travel.sh init"
	# shellcheck disable=SC1090
	. "$ENV_FILE"
}

binary() {
	if [ -n "${JELLYBLOOM:-}" ]; then
		echo "$JELLYBLOOM"
	elif command -v jellybloom >/dev/null 2>&1; then
		command -v jellybloom
	elif [ -f "$REPO/go.mod" ] && command -v go >/dev/null 2>&1; then
		mkdir -p "$TRAVEL_DIR/bin"
		(cd "$REPO" && go build -o "$TRAVEL_DIR/bin/jellybloom" ./cmd/jellybloom) >&2
		echo "$TRAVEL_DIR/bin/jellybloom"
	else
		die "no jellybloom binary: set JELLYBLOOM or put jellybloom on PATH"
	fi
}

# users prints the users: section for the configs.
users() {
	if [ -n "${USERS_FROM:-}" ]; then
		[ -f "$USERS_FROM" ] || die "USERS_FROM: no such file: $USERS_FROM"
		block=$(awk '/^users:/ { on = 1; print; next } on && /^[^ \t#-]/ { on = 0 } on' "$USERS_FROM")
		[ -n "$block" ] || die "no users: section in $USERS_FROM"
		echo "$block" | grep -q 'admin: *true' || echo "travel: note: no admin user in $USERS_FROM; sync and the report need one" >&2
		echo "$block"
		return
	fi
	printf 'JellyBloom user name (an admin, used for sync and reports): ' >&2
	read -r name
	[ -n "$name" ] || die "a user name is required"
	echo "Password for $name:" >&2
	hash=$("$JB" hash-password) || die "hash-password failed"
	printf 'users:\n  - {name: "%s", password_hash: "%s", admin: true}\n' "$name" "$hash"
}

# config writes one instance's config.
config() { # instance listen merge-lines
	mkdir -p "$TRAVEL_DIR/$1/data"
	chmod 700 "$TRAVEL_DIR/$1/data"
	cat > "$TRAVEL_DIR/$1/config.yaml" <<EOF
# Written by scripts/travel.sh init ($1). Edit freely; init overwrites it.
listen: "$2"
server_name: "$3"
data_dir: "$TRAVEL_DIR/$1/data"
$4
# The laptop's server is listed first: it wins ties until speeds are measured,
# and it serves the web app, which then works with no internet.
web_upstream: $LAPTOP_NAME
upstreams:
  - {name: $LAPTOP_NAME, url: "$LAPTOP_URL"}
  - {name: $HOME_NAME, url: "$HOME_URL"}
$USERS
EOF
	chmod 600 "$TRAVEL_DIR/$1/config.yaml"
}

init() {
	[ -n "${HOME_URL:-}" ] || die "set HOME_URL to your home Jellyfin server (e.g. HOME_URL=https://jellyfin.example.com)"
	HOME_NAME=${HOME_NAME:-home}
	LAPTOP_URL=${LAPTOP_URL:-http://127.0.0.1:8096}
	LAPTOP_NAME=${LAPTOP_NAME:-laptop}
	[ "$HOME_NAME" != "$LAPTOP_NAME" ] || die "HOME_NAME and LAPTOP_NAME must differ"
	mkdir -p "$TRAVEL_DIR"
	chmod 700 "$TRAVEL_DIR"
	JB=$(binary)
	USERS=$(users)
	if [ -z "${SYNC_USER:-}" ]; then
		SYNC_USER=$(echo "$USERS" | awk '/name:/ { n = $0 } /admin: *true/ { print n; exit }' |
			sed -E 's/.*name: *"?([^",}]*)"?.*/\1/')
	fi

	config library "${LIBRARY_LISTEN:-:8400}" "Traveling Library" "merge: union"
	config packing "${PACKING_LISTEN:-127.0.0.1:8401}" "Not packed yet" "merge: difference
base: $HOME_NAME
subtract: [$LAPTOP_NAME]"
	config unpacking "${UNPACKING_LISTEN:-127.0.0.1:8402}" "To bring home" "merge: difference
base: $LAPTOP_NAME
subtract: [$HOME_NAME]"

	# Pair once (into the library's data), then share it with the lists.
	secrets=$TRAVEL_DIR/library/data/secrets
	mkdir -p "$secrets"
	chmod 700 "$secrets"
	for name in $LAPTOP_NAME $HOME_NAME; do
		if [ -n "${PAIRING_FROM:-}" ] && [ -f "$PAIRING_FROM/upstream-$name" ]; then
			cp "$PAIRING_FROM/upstream-$name" "$secrets/"
		elif [ ! -f "$secrets/upstream-$name" ]; then
			echo "Pairing with $name: approve the code below in a Jellyfin app signed in to that server."
			"$JB" pair -config "$TRAVEL_DIR/library/config.yaml" "$name"
		fi
	done
	for inst in packing unpacking; do
		mkdir -p "$TRAVEL_DIR/$inst/data/secrets"
		chmod 700 "$TRAVEL_DIR/$inst/data/secrets"
		cp "$secrets"/upstream-* "$TRAVEL_DIR/$inst/data/secrets/"
	done

	cat > "$ENV_FILE" <<EOF
HOME_URL='$HOME_URL'
HOME_NAME='$HOME_NAME'
LAPTOP_URL='$LAPTOP_URL'
LAPTOP_NAME='$LAPTOP_NAME'
HOME_JELLYBLOOM='${HOME_JELLYBLOOM:-}'
SYNC_USER='$SYNC_USER'
JELLYBLOOM='$JB'
EOF
	chmod 600 "$ENV_FILE"
	echo "Set up in $TRAVEL_DIR. Next: scripts/travel.sh start"
}

port() { sed -n 's/^listen: *"\{0,1\}[^:]*:\([0-9]*\)"\{0,1\}/\1/p' "$TRAVEL_DIR/$1/config.yaml"; }

running() { [ -f "$TRAVEL_DIR/$1/pid" ] && kill -0 "$(cat "$TRAVEL_DIR/$1/pid")" 2>/dev/null; }

start() {
	load
	for inst in $INSTANCES; do
		if running "$inst"; then
			echo "$inst already running"
			continue
		fi
		nohup "$JELLYBLOOM" -config "$TRAVEL_DIR/$inst/config.yaml" > "$TRAVEL_DIR/$inst/jellybloom.log" 2>&1 &
		echo $! > "$TRAVEL_DIR/$inst/pid"
	done
	for inst in $INSTANCES; do
		p=$(port "$inst")
		i=0
		until curl -s -m 1 -o /dev/null "http://127.0.0.1:$p/System/Info/Public"; do
			i=$((i + 1))
			[ $i -lt 30 ] || die "$inst did not start; see $TRAVEL_DIR/$inst/jellybloom.log"
			sleep 0.5
		done
		echo "$inst: http://127.0.0.1:$p"
	done
}

stop() {
	load
	for inst in $INSTANCES; do
		if running "$inst"; then
			kill "$(cat "$TRAVEL_DIR/$inst/pid")" && echo "stopped $inst"
		fi
		rm -f "$TRAVEL_DIR/$inst/pid"
	done
}

status() {
	load
	for inst in $INSTANCES; do
		p=$(port "$inst")
		if running "$inst" && curl -s -m 2 -o /dev/null "http://127.0.0.1:$p/System/Info/Public"; then
			echo "$inst: running on :$p"
		else
			echo "$inst: stopped"
		fi
	done
	"$JELLYBLOOM" report -config "$TRAVEL_DIR/library/config.yaml" | head -1
}

sync_state() {
	load
	[ -n "${HOME_JELLYBLOOM:-}" ] || die "set HOME_JELLYBLOOM in $ENV_FILE to your home JellyBloom's URL"
	[ -n "${SYNC_USER:-}" ] || die "set SYNC_USER in $ENV_FILE to an admin user that exists on both"
	running library || die "the library is not running: scripts/travel.sh start"
	set -- -user "$SYNC_USER"
	[ -z "${SYNC_PASSWORD_FILE:-}" ] || set -- "$@" -password-file "$SYNC_PASSWORD_FILE"
	"$JELLYBLOOM" sync "$@" "http://127.0.0.1:$(port library)" "$HOME_JELLYBLOOM"
}

case "${1:-}" in
	init) init ;;
	start) start ;;
	stop) stop ;;
	status) status ;;
	sync) sync_state ;;
	report) load; shift; "$JELLYBLOOM" report -config "$TRAVEL_DIR/library/config.yaml" "$@" ;;
	*) sed -n '2,13p' "$0" | sed 's/^# \{0,1\}//' >&2; exit 2 ;;
esac
