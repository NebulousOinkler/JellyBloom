# Test rig

Two real Jellyfin servers on this machine, with JellyBloom in front of them. Native
processes (no Docker): uses the Jellyfin app's server and bundled ffmpeg.

| Service | Address | Content |
|---------|---------|---------|
| Jellyfin A | http://127.0.0.1:8201 | Movies (incl. a shared movie with a 480p second version), Shows |
| Jellyfin B | http://127.0.0.1:8202 | Music, Movies (the shared movie, byte-identical to A's) |
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
testrig/servers.sh stop
```

Each server gets a non-admin `jellybloom` account; `setup.py` pairs JellyBloom with it
through `jellybloom pair` (approving the Quick Connect code via the API), so the generated
config holds no upstream secrets. Credentials are generated on first setup and stored in
`testrig/data/rig.json`;
everything under `testrig/data/` is git-ignored. JellyBloom logs at debug level to
`testrig/data/jb/jellybloom.log`. `testrig/inspect.py` lists what each server indexed.
