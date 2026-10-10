# JellyBloom

Presents several Jellyfin servers as one Jellyfin server. Clients (jellyfin-web, the mobile
and TV apps) connect to JellyBloom as if it were Jellyfin, and see the servers' libraries
combined by a set operation:

- `union`: everything on any server
- `intersection`: only what is on every server
- `difference`: what is on `base` and on none of the `subtract` servers

Copies of the same item on several servers become one item. JellyBloom keeps every copy:
it plays from the fastest healthy one, fails over to another copy mid-stream when a server
or a file breaks, and reports what exists on only one server. It is a pure proxy: media
never touches its disk, only a metadata index does.

## Status
Phases 1 to 9 of the [build plan](docs/PHASES.md) are done: browsing, playback, user state
(played, resume, Next Up, favorites, playlists, collections), redundancy (copy checks,
ranking, failover, health, report), set operations, and hardening. Tested against Jellyfin
10.9 to 12.2 on a local two-server rig (`testrig/`).

## Run
    cp config.example.yaml config.yaml        # add your upstreams and a user
    go run ./cmd/jellybloom hash-password      # for the user's password_hash
    go run ./cmd/jellybloom pair -config config.yaml home   # once per upstream
    go run ./cmd/jellybloom -config config.yaml

Then point a Jellyfin client at `http://host:8096`.

### Container (podman or docker)
    podman build -t jellybloom -f Containerfile .
    podman run --rm -it -v ./config.yaml:/config/config.yaml:ro -v jellybloom-data:/data \
      jellybloom pair -config /config/config.yaml home
    podman run -d --name jellybloom -p 8096:8096 \
      -v ./config.yaml:/config/config.yaml:ro -v jellybloom-data:/data jellybloom

In the container, set `data_dir: /data`. The image is static and runs as a non-root user.

## Operating
- `jellybloom report -config config.yaml` lists items with a good copy on only one server,
  and broken, disputed, or quarantined copies, with their file paths. Admin users
  (`admin: true`) get the same, with live server state, at `GET /JellyBloom/Report`.
- `metrics_listen: 127.0.0.1:9101` serves Prometheus metrics on a separate address:
  requests, upstream health, latency and throughput, failovers, relay bytes, sync state.
- `log_format: json` and `log_level: debug` for structured logs; every request is logged
  at debug level with IDs templated and no query string (tokens never reach the log).
- `upstream_timeout` (default 15s) bounds how long a server may take to answer before
  JellyBloom tries another or gives up; `stall_timeout` (default 10s) is how long a stream
  may stop before failing over.

## The traveling library
`scripts/travel.sh` sets up JellyBloom on a laptop that carries part of your library:
local copies play first, home fills the gaps, it works offline, and packing and unpacking
lists come for free. `jellybloom sync` keeps watch history the same on the laptop and at
home. See [docs/TRAVELING-LIBRARY.md](docs/TRAVELING-LIBRARY.md).

## Design
See [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) for the architecture, and
[testrig/README.md](testrig/README.md) for the test rig.
