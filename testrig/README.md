# Test rig

Two real Jellyfin servers on this machine, with JellyBloom in front of them. Native
processes (no Docker): uses the Jellyfin app's server and bundled ffmpeg.

| Service | Address | Content |
|---------|---------|---------|
| Jellyfin A | http://127.0.0.1:8201 | Movies (a shared movie with a 480p second version; a 10-minute movie), Shows (two seasons), a collection |
| Jellyfin B | http://127.0.0.1:8202 | Music (incl. a 6-minute track), Movies (both shared movies, byte-identical to A's; a damaged and a truncated copy of two more), Books (an audiobook) |
| JellyBloom | http://127.0.0.1:8200 | Union of A and B; stock jellyfin-web at `/web` |

All media is synthetic (generated with ffmpeg). Everything binds to 127.0.0.1.

## Use

```sh
brew install --cask jellyfin   # once
testrig/media.sh               # generate media (idempotent)
testrig/servers.sh start       # start A, B, and JellyBloom
testrig/setup.py               # wizard, libraries, scan; writes JellyBloom config (idempotent)
testrig/servers.sh restart-jb  # pick up the generated config / rebuilt code
testrig/smoke.py               # drive JellyBloom like a client; PASS/FAIL per check
testrig/compare.py             # same user-state steps on real Jellyfin and JellyBloom; diff
testrig/servers.sh stop-server b   # simulate an upstream outage (start-server b to recover)
testrig/servers.sh stop
```

Each server gets a non-admin `jellybloom` account; `setup.py` pairs JellyBloom with it
through `jellybloom pair` (approving the Quick Connect code via the API), so the generated
config holds no upstream secrets. Credentials are generated on first setup and stored in
`testrig/data/rig.json`;
everything under `testrig/data/` is git-ignored. JellyBloom logs at debug level to
`testrig/data/jb/jellybloom.log`. `testrig/inspect.py` lists what each server indexed.
