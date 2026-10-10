# Test rig

Two real Jellyfin servers on this machine, with JellyBloom in front of them. Native
processes (no Docker): uses the Jellyfin app's server and bundled ffmpeg.

| Service | Address | Content |
|---------|---------|---------|
| Jellyfin A | http://127.0.0.1:8201 | Movies (a shared movie with a 480p second version; a 10-minute movie), Shows (two seasons), a collection |
| Jellyfin B | http://127.0.0.1:8202 | Music (incl. a 6-minute track), Movies (both shared movies, byte-identical to A's; a damaged and a truncated copy of two more), Books (an audiobook) |
| Fault proxy | :8211 → A, :8212 → B | How JellyBloom reaches A and B; injects faults on demand (below) |
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
testrig/failover.py            # copy checks, ranking, and failover under injected faults
testrig/setops.py              # intersection, a \ b, b \ a instances checked against the servers
testrig/servers.sh stop-server b   # simulate an upstream outage (start-server b to recover)
testrig/servers.sh faults a '{"cut_after": 30000000}'   # inject faults on a (clear: faults a clear)
testrig/servers.sh stop
```

Each server gets a non-admin `jellybloom` account; `setup.py` pairs JellyBloom with it
through `jellybloom pair` (approving the Quick Connect code via the API), so the generated
config holds no upstream secrets. Credentials are generated on first setup and stored in
`testrig/data/rig.json`;
everything under `testrig/data/` is git-ignored. JellyBloom logs at debug level to
`testrig/data/jb/jellybloom.log`. `testrig/indexed.py` lists what each server indexed.

## Faults

JellyBloom reaches the servers through `testrig/faultproxy` (a on :8211, b on :8212), which
passes everything through until told otherwise. `servers.sh faults a|b` shows a listener's
faults and counters; give it JSON to set faults (omitted fields are cleared) or `clear`:

| Field | Effect |
|-------|--------|
| `down` | every connection is closed at once |
| `latency_ms` | delay before each request |
| `throttle_kbps` | media bodies at most this fast |
| `cut_after` | media bodies break off after this many bytes |
| `stall_after` | media bodies stop sending after this many bytes |
| `status` | media requests get this HTTP status |

JellyBloom's metrics are at http://127.0.0.1:8209/metrics. `setops.py` starts its extra
instances on :8301 to :8303 with copies of the main instance's pairing, under
`testrig/data/setops/`, and stops them when done.

Media requests are file streams, downloads, and HLS. The rig config sets `stall_timeout: 3s`
so stalls fail over quickly, and makes the rig user a JellyBloom admin, so
`/JellyBloom/Report` works (`jellybloom report -config testrig/data/jellybloom.yaml` prints
the same from the database).
