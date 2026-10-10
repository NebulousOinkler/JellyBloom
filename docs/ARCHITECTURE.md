# JellyBloom Architecture & Build Plan

JellyBloom presents several Jellyfin servers ("upstreams") as one Jellyfin server.
Clients, the stock jellyfin-web UI, and plugins on the upstreams should not be able
to tell that the content comes from more than one server.

## Request paths

Every client request takes one of four paths:

| Path | Behavior | Examples |
|------|----------|----------|
| **Local** | Answered from JellyBloom's index and stores | Views, `/Items` lists, search, user data, sessions, auth |
| **Forward + rewrite** | Sent to one upstream; IDs/URLs/tokens rewritten both ways | `/Items/{id}`, `PlaybackInfo`, HLS playlists, images |
| **Relay** | Bytes streamed through unchanged (Range supported) | Video/audio streams, HLS segments, subtitles |
| **Generic fallback** | Unknown route; owning upstream inferred from item IDs in the request | Plugin endpoints, core endpoints not yet implemented |

The client only ever sees JellyBloom's host, IDs, and tokens.

## Invariant: no local storage of upstream files

JellyBloom is a pure proxy. It never writes upstream files to disk: no media, no
segments, no subtitles, no artwork.

| JellyBloom persists | JellyBloom never persists |
|---------------------|---------------------------|
| Item metadata (DTO JSON) in the index | Video and audio files, or any part of them |
| ID mappings | HLS playlists and segments |
| Version digests (sampled-block and 1 MiB block hashes) | Subtitle files |
| JellyBloom's own users, tokens, user data, playlists | Images and artwork |
| Source health: quarantines and content verdicts (speed stats stay in memory) | Transcode output |

Rules for implementation:
- **Relays stream from memory.** Upstream bodies are copied to the client through a fixed,
  bounded buffer (`io.CopyBuffer`), with no temp files and no spooling. Failover reissues a
  `Range` request to the next source instead of replaying buffered data.
- **Rewritten text** (JSON, m3u8, HTML, JS) is rewritten in memory per response, and
  never cached on disk.
- **Hashing streams through.** Partial content hashes read the bytes through the hash
  function and keep only the digest.
- **Caching is delegated to clients.** Upstream `Cache-Control`, `ETag`, and
  `Last-Modified` headers pass through, so clients and browsers cache artwork themselves.
  `ImageTags` already change whenever an image changes. (Relayed files drop `ETag` and
  `Last-Modified`, which differ between upstreams for the same file; see Failover.)
- **Enforcement:** `internal/storagecheck` parses every non-test Go file and fails on
  file-writing calls (`os.Create`, `CreateTemp`, `WriteFile`, `OpenFile`, `MkdirTemp`,
  `Rename`, …) and on multipart parsing (which spools to temp files) outside
  `internal/secrets`, which stores upstream tokens. `TestNoLocalStorage` plays media through
  JellyBloom (relays with failover, HLS, artwork, user data, a playlist) with `TMPDIR`
  redirected, then checks that only the SQLite files exist and that they stayed far smaller
  than the media relayed. CI runs both on every push.

## Core concepts

### Upstream sessions
JellyBloom logs in to each upstream as a normal client (`POST /Users/AuthenticateByName`
with a `MediaBrowser Client="JellyBloom", Device=…, DeviceId=…, Version=…` header) and
sends `Token=…` on every later call. Upstreams configured with an `api_key` must also name
a `user`, because item queries need a user context.

### Index
SQLite index of every upstream item. It stores the full DTO JSON per `(upstream, realId)`.
- **Full sync:** pages through `GET /Items?Recursive=true&Fields=…`.
- **Incremental sync:** listens on the upstream's `/socket` for `LibraryChanged` messages
  and re-fetches the affected IDs, with a periodic resync as a backstop. Jellyfin ignores
  `ParentId` when `Ids` is given, so each changed item's library comes from
  `/Items/{id}/Ancestors`; an item in a library the index lacks (a new library), a change
  to a library itself, or a burst of more than 200 items triggers a full sync instead.
- List, search, latest, resume, and next-up queries run as SQL over the merged index, so
  sort, filter, `StartIndex`/`Limit`, and `TotalRecordCount` are exact.

### Identity and merge
- Each item type has a **match key**:

  | Type | Match key |
  |------|-----------|
  | Movie | TMDb/IMDb ID, else title + year |
  | Episode | Series provider ID + season/episode number |
  | Album | MusicBrainz release group ID, else album artist + title |
  | Track | MusicBrainz recording ID, else album + track number + title |
  | No reliable key | Never deduplicated |
- **Unified ID:** `UUIDv5(ns, matchKey)`, or `UUIDv5(ns, upstream+":"+realId)` when there
  is no key. IDs are written in Jellyfin's 32-hex GUID format and are deterministic across
  restarts.
- **Mapping table:** `unifiedId → [(upstream, realId), …]`. It keeps **every** copy;
  merging never discards a source (see [Redundant sources](#redundant-sources)).
- **Set operations** run over the match-key groups and decide only which unified items are
  visible. A visible item keeps all of its copies on the participating upstreams.

  | Op | Visible items | Sources kept |
  |----|---------------|--------------|
  | `union` | On any upstream | All copies on all upstreams |
  | `intersection` | On every upstream | All copies on all upstreams |
  | `difference` | On `base` and not on any `subtract` upstream | All copies on `base` |

  `difference` is A \ B: `base` names A, and `subtract` names B (one or more upstreams).
- **Metadata conflicts:** the preferred upstream (config order) wins, and the other
  upstreams fill any gaps.

### Views
`/UserViews` is synthesized by grouping upstream libraries by `CollectionType`. A music-only
upstream and a video-only upstream together produce Music, Movies, and Shows views.
Libraries with no visible items (for example, one only a subtracted upstream has) are
left out.

### Redundant sources
A unified item usually has several copies across upstreams. JellyBloom keeps all of them
and uses them for three things: playing from the fastest copy, routing around broken
files, and keeping items available when an upstream is down. `internal/sources` owns
this.

**Versions vs. sources.** Copies are grouped in two levels:
- A **version** is a distinct encode. Copies are one version when they match on
  container, runtime, stream layout (codecs, resolution, channels), and file size, and,
  where it can be checked, on content: a **fingerprint** of 17 sampled 64 KiB blocks
  (the first, the last, and 15 spread evenly, so one sits at the middle), read with Range
  requests and streamed through SHA-256. Only the 8-byte digests are stored. Small files
  are fingerprinted whole.
- A **source** is one location of a version: `(upstream, realItemId, realMediaSourceId)`.

`PlaybackInfo` shows the client one `MediaSource` per **version**, so clients display
"1080p HEVC" and "4K HDR" as normal Jellyfin versions. Version names describe the encode
and never the upstream. Each version's unified `MediaSource.Id` maps to its ordered list of
sources.

**Content checks.** Fingerprints are taken where grouping depends on them: two or more
copies that look identical, or an encode present in more than one size. A background
verifier fingerprints every multi-copy item (one item at a time, paced), resamples
fingerprints older than 30 days, and forgets copies that are gone; `PlaybackInfo` checks
what is still missing, bounded to 5 seconds. Checks produce **verdicts**, persisted:
- *Truncated:* a smaller copy of the same encode whose sampled blocks match the larger
  copy's bytes at the same offsets. (Jellyfin reports the header's runtime, so a cut-off
  file otherwise looks like a second version.)
- *Damaged:* same size, but a sample differs, and this copy's block is mostly zero bytes
  where the other has data (unreadable sectors, file holes, and failed copies read back
  as zeros; encoders pad with `0xff`, never zeros). With three or more copies, the minority
  loses. Copies that differ in most samples are different encodes, not damage.
- *Disputed:* two copies differ with no evidence either way. They become separate versions
  (never spliced), and the dispute is re-decided daily (a third copy may settle it).

Sampling catches damage only where a sample lands. **Block hashes taken during playback**
close the gap at no extra traffic: while relaying a copy that has identical peers, JellyBloom
hashes each whole 1 MiB block it passes through and stores the hash. When a peer's stored
hash for the same block differs, the block is re-read from every copy and settled the same
way, and a relay on the damaged copy moves to a good one at once.

A broken copy is left out of `PlaybackInfo` when a good copy of its encode is available.
When it is the only copy available, it is still offered and played, and the report lists
it. Damaged verdicts are re-read daily and dropped if the file was replaced; a truncated
verdict lapses when the copy's size changes.

**Source ranking** within a version is re-evaluated per play request:
1. Health: healthy; then a recent failure (forgiven after 10 minutes); then an unreachable
   upstream; then quarantined; then broken.
2. Speed: the expected time to fetch 4 MiB from the upstream, from latency (pings every 30
   seconds, and response times) and throughput (measured on relays, only in windows where
   reading from the upstream, not writing to the client, was the bottleneck). An upstream
   must be 25% faster to pass one earlier in the config, so near-equal upstreams don't flap.
   An unmeasured upstream counts as fastest, so each gets tried and measured once.
3. Config order.

Version choice, when the client doesn't pick one, follows the client's `DeviceProfile`:
direct-play-compatible first, then quality.

**Failover.**
- *Files as stored (direct play with `static=true`, downloads):* JellyBloom relays these
  itself. A source that errors, stalls (no bytes for `stall_timeout`, default 10 s), or
  answers with an error before responding is skipped. One that breaks off, stalls, or is
  found damaged mid-body is replaced by an identical copy (same fingerprint, no verdict):
  JellyBloom requests the rest of the span (`Range: bytes=pos-last`), checks that the
  `Content-Range` matches, and keeps writing. The client sees one uninterrupted response.
  Upstream `ETag`/`Last-Modified` are dropped, since they differ between upstreams for the
  same file; conditional request headers are never forwarded.
  Because a client stitches range requests for one URL together, every response for a
  version comes from copies of the same file: a version from `PlaybackInfo` (which grouped
  them), or else only copies with the same fingerprint as the best one.
- *Everything else (HLS, remuxes, subtitles, reports, plugin routes):* if the chosen
  upstream cannot be reached, or answers a GET with a 5xx, before anything was sent to the
  client, the request is retried on the next upstream holding the items. For HLS, Jellyfin
  starts a transcode for any requested segment, so the next upstream's transcode serves
  the following segments with the same numbers; the client may see a brief re-buffer.
  IDs minted for one upstream's opaque values (such as a media source `Tag` in segment
  URLs) only prefer that upstream; they never block a retry.
- *Different versions:* JellyBloom never switches versions silently. If every source of the
  chosen version fails, playback errors, and the client can pick another version.

**Health.** A source that fails three times within 10 minutes is quarantined for one
minute, doubling on each failed re-check up to an hour. When the backoff ends, a one-byte
read re-checks it; serving successfully ends a quarantine at any time. Quarantines are
persisted. Connection failures mark the whole upstream unreachable until a request or ping
succeeds; `PlaybackInfo` skips unreachable upstreams, and waits at most 3 seconds for slower
upstreams once the first has answered.

**Availability.** An item stays visible and playable while any source is reachable.
Upstream outages only reduce redundancy.

**Redundancy report.** `GET /JellyBloom/Report` (JellyBloom users with `admin: true`) and
`jellybloom report` (from the database; `-json`, `-all`). It lists items with a good copy
on only one upstream, broken, disputed, and quarantined copies with the upstream's file
path, totals including multi-copy items not checked yet, and (from the server) each
upstream's reachability, latency, and throughput. JellyBloom does not copy files itself;
the report shows what needs backing up or replacing.

**Limitation:** media bytes flow through JellyBloom, so "fastest source" means fastest to
JellyBloom. Regional benefit comes from placing JellyBloom near its clients, or running
one JellyBloom instance per region with the same config.

### Active source
Each play session (`PlaySessionId` from `PlaybackInfo`, or the user's device and item when
a request has none) has an **active upstream**, the one serving it:
- It is set by the first relay, and stays while that upstream is as healthy as the best
  one, so a session's requests keep going to one place.
- Streams, HLS, subtitles, progress reports, media segments, and generic-fallback calls for
  that item all prefer the active upstream. A failover (byte-level or a retried request)
  moves the session, and everything after it follows. Failover only moves between
  identical files, so plugin data such as intro timestamps still matches the file being
  played.
- Sessions are kept in memory and expire after 12 idle hours.

### Rewriting (applies to every forwarded response)
- **Requests:** unified IDs → real IDs, and the client token → the upstream token.
- **Responses, text bodies only (JSON, m3u8, HTML, JS):**
  - Every GUID that is a known real ID on that upstream → its unified ID.
  - An unknown GUID gets a lazy mapping, `UUIDv5(upstream:id)`, recorded in the mapping
    table, so it can be routed later. Such *minted* IDs (cache tags and the like) prefer
    their upstream when routing but never keep a request from failing over; *aliases*
    JellyBloom sets up (versions, play sessions, merged libraries) do constrain routing.
    Play session aliases are pruned after 7 days.
  - The upstream base URL and token are stripped.
  - File paths (`Path` in item, media source, and stream DTOs) become neutral:
    `/media/` plus the last three parts for a file (library folder, item folder, file
    name) or two for a folder, so no upstream's directory layout shows and copies of a file
    look alike everywhere. URLs of remote streams are left alone. The admin report keeps
    the real paths.
- **Binary bodies** pass through untouched.

### Playback
1. `POST /Items/{id}/PlaybackInfo` goes to every upstream holding the item. Their
   `MediaSources` are grouped into versions and ranked. Each version gets a unified
   `MediaSource.Id` that maps to its source list. `TranscodingUrl` is rewritten, and
   `PlaySessionId → active source` is recorded.
2. **Direct play** (`/Videos/{id}/stream?static=true`, `/Audio/{id}/stream?static=true`,
   `/Items/{id}/Download`): relayed by the source registry with the `Range` header, with
   byte-level failover across identical sources. Other streams (remux, `/Audio/{id}/universal`)
   take the generic path, with retry before the response starts.
3. **HLS:** playlists are rewritten line by line (URIs, `URI="…"` attributes, `api_key`).
   Segments are routed by `PlaySessionId` or `MediaSourceId`.
4. **Progress** (`/Sessions/Playing*`): recorded locally *and* forwarded upstream, so the
   upstream ends its transcodes (`Stopped`, `DELETE /Videos/ActiveEncodings`).

### Owned by JellyBloom
- Users and tokens. Tokens are accepted from the `Authorization: MediaBrowser … Token=`
  header, `X-Emby-Token`, `X-MediaBrowser-Token`, and the `api_key` query parameter.
- User data: played, favorite, resume, play count. It is overlaid onto every DTO's
  `UserData`. Both the 10.9+ and the legacy route styles are supported.
- Playlists and collections.
- `/System/Info(/Public)`, branding, display preferences, a single stable `ServerId`.
- Client WebSocket `/socket`: KeepAlive, plus `LibraryChanged` and `UserDataChanged`.
- Case-insensitive routing.

### Partial failure
One upstream failing, hanging, or answering slowly must only cost what that upstream alone
can serve:
- **Timeouts per request type.** An upstream must start answering an API request within
  `upstream_timeout` (15 s), media requests (streams, HLS, subtitles, PlaybackInfo) within
  60 s (transcodes start slowly), and an upstream already known to be unreachable within
  5 s. Bodies then stream untimed (relays have their own stall guard).
- **Retry elsewhere.** A request that times out or fails to connect is retried on the next
  upstream holding the items, and the upstream is marked unreachable, so later requests go
  elsewhere first; a request or a ping (every 30 s) that succeeds marks it reachable again.
- **Device sessions.** Giving a client device its own upstream session (Quick Connect) is
  bounded to 10 s; if it fails, the request uses JellyBloom's own session and minting is
  retried a minute later.
- **Fan-out.** PlaybackInfo asks reachable upstreams only and waits at most 3 s for slower
  ones after the first answers; upstream collections and other fan-outs are bounded by the
  request timeouts.
- Everything answered from the index (libraries, lists, search, details, user state) never
  waits on an upstream.

On the rig, with one server hanging, browsing stays instant; a request only that server can
answer gives up within 25 s the first time and 5 s after.

### Observability
- **Logs:** `slog`, text or JSON, with every attribute passed through credential redaction.
  Each request is logged at debug level (5xx at warn): method, path with IDs templated, the
  matched route, status, bytes, time to first byte, total time. Query strings, which carry
  tokens, are never logged.
- **Metrics** (`metrics_listen`, Prometheus text format, a separate address so the client
  port stays identical to Jellyfin's): client requests by route and status, time to first
  byte, response bytes; upstream requests by outcome, retries, reachability, latency, and
  throughput; relay bytes and failovers; quarantined and flagged copies; play sessions;
  index items and sync state per upstream; visible items and libraries; socket clients.

## Minimal generic routing (plugins and unimplemented endpoints)

Jellyfin plugins are .NET code running inside the upstream servers, so they stay installed
there. JellyBloom forwards their HTTP traffic per item, so each plugin only ever sees
requests about its own items, with its own IDs.

**Fallback algorithm**, for any route without a dedicated handler:
1. **Extract** GUID-shaped tokens (32-hex or dashed) from the path segments, the query
   values, and the JSON body. Look each one up in the mapping table.
2. **Route:**
   - Item references found → the **active source** of the item. If the references point
     to different upstreams, use the first item's active source and log the conflict.
   - No item references → the **primary upstream** (first in config order).
3. **Rewrite the request**: IDs and token, as above.
4. **Rewrite the response**: generic GUID/URL/token rewriting, as above.
5. **Log** the method, route template, client, and chosen upstream. This log decides which
   routes deserve dedicated handlers.

**Web UI:** `/web/*` is proxied to the `web_upstream` (defaults to the primary upstream),
with the same rewriting. That serves the stock jellyfin-web, including that upstream's
injected plugin scripts and config pages.

**Deferred** (until the log shows a real need):
- Merging plugin responses across upstreams. Library-wide plugin routes go to the primary
  upstream only.
- Exposing each upstream's plugin config and admin pages.
- Plugin-specific handlers.

**Known limits:**
- Plugins that embed IDs in a form the scan doesn't recognize (base64 or other encodings).
  These degrade per feature and are fixed case by case.
- Installing plugins on JellyBloom itself is out of scope, because that would require
  JellyBloom to be a .NET Jellyfin host.

## User state

JellyBloom owns each user's watch state, keyed by unified item ID, so it is the same
whichever upstream serves an item:
- played, play count, favorite, likes, resume position, last played, following Jellyfin
  12.2 exactly (checked with `testrig/compare.py`): progress and stop reports apply the
  resume rules (under 5% no resume point, over 90% played, items under 5 minutes played);
  audiobooks use 5-minute rules instead; music is played when it starts and never keeps a
  resume point; a start counts a play but leaves the position alone; marking unplayed
  clears play count and date; a played item with a resume point (a rewatch) is resumable
- folders are played when all their playable items are; marking one marks its items, and
  marking a collection or playlist marks its members
- Next Up continues after the furthest played episode of each series being watched, and
  honors `enableResumable`, `enableRewatching`, and `nextUpDateCutoff`
- playback reports update local state and are forwarded upstream (transcode cleanup)
- Resume, Next Up, favorites, and played filters/sorts read local state
- playlists (private, ordered; `SortBy` reorders, `Random` shuffles) and collections
  (shared) hold unified IDs. Upstream collections (BoxSets) link their members, so members
  are asked of the upstreams when browsed; edits to them are kept in JellyBloom on top of
  the upstream's members and never sent upstream
- when items merge (identity redirect), their user data and list entries move to the new ID

## Upstream credentials

JellyBloom needs upstream *tokens*, never passwords. Jellyfin tokens do not expire, can
be revoked per device from the server's dashboard, and do not expose a password that may
be reused elsewhere.

- **Pairing (default):** `jellybloom pair NAME` starts Quick Connect and prints a code.
  Someone signed in to that server as the intended account approves it in their Jellyfin
  app; JellyBloom receives a token and never sees the password. This also suits friends'
  servers: the friend approves on their side.
- **Fallbacks:** `pair -password` exchanges a password once (it is not kept);
  `password_file` does the same unattended; `token_command` reads the credential from a
  password manager; `pair -api-key` stores a server API key (admin-level, opt-in only).
- **Secret store:** tokens live outside the config, in owner-only files under
  `data_dir/secrets` (JellyBloom refuses to start if they are readable by others) or in
  the OS keychain (`secret_store: keychain`). The config holds no secrets.
- **Least privilege:** each login checks the account via `/Users/Me`; administrator
  accounts and API keys are refused unless the upstream sets `allow_admin: true`, at
  pairing time and at every startup. JellyBloom blocks admin routes anyway, so admin
  rights only add risk.
- **Per-client devices:** each (JellyBloom user, client device) gets its own upstream
  session, minted by approving a Quick Connect code with the paired token. Without Quick
  Connect it falls back to `password_file`, then to one shared session (with a warning).
- **Revocation:** re-pairing revokes the token it replaces; `jellybloom unpair NAME`
  revokes and deletes. If an upstream revokes JellyBloom, requests fail with a clear
  "re-pair" error (Jellyfin rejects revoked sockets with 403, other calls with 401), and a
  new pairing is picked up without a restart.
- **Hygiene:** every log line passes through credential redaction (URL `ApiKey=`,
  `Token="…"` headers, JSON token/password fields); Go transport errors otherwise embed
  full URLs. JellyBloom's own session tokens are stored only as SHA-256 hashes.

## Out of scope (initially)
- Live TV / DVR
- SyncPlay
- Admin dashboard and scheduled tasks. Admin routes return 403 or 404.

## Build plan

| # | Phase | Deliverables | Done when |
|---|-------|-------------|-----------|
| 1 | **Upstream client** | Login, token handling, paged `/Items` fetch, upstream `/socket` listener, health checks, retry with backoff | Lists items from a real or mock Jellyfin in tests |
| 2 | **Index and sync** | SQLite schema, full sync, incremental sync, periodic resync | Index matches upstream after edits |
| 3 | **Identity** | Match-key registry per type, UUIDv5 IDs, mapping table that keeps all copies, lazy mappings, set-operation engine (`difference` = `base` \ `subtract`) | Same item on two servers yields one ID with both sources; table tests pass for all set operations |
| 4 | **Server skeleton and generic routing** | Auth and token extraction, `/System/*`, `/socket`, case-insensitive router, **generic fallback router**, **response rewriter**, **`/web` proxy to `web_upstream`**, unhandled-route log | Stock jellyfin-web loads from JellyBloom and logs in; unknown routes reach the right upstream |
| 5 | **Browse (union only)** | `/UserViews` synthesis, `/Items` queries, item details, search, images, `Fields=` trimming | **Milestone:** a music-only and a video-only upstream browse as one server |
| 6 | **Playback** | `PlaybackInfo` merge with **version/source grouping**, source ranking, **active-source tracking**, direct-play relay, HLS rewriting, `PlaySessionId` routing, progress forwarding | Video and music play and seek; duplicate encodes show as one version; transcodes are cleaned up on stop |
| 7 | **User state** | Played, favorites, resume, next-up, playlists, collections | State persists across upstreams and devices |
| 8 | **Redundancy** | Partial content hashing, throughput and latency stats, byte-level failover, HLS failover, health tracking and quarantine, redundancy report | Killing an upstream or corrupting a file mid-play continues playback from another copy |
| 9 | **Set operations and hardening** | Intersection and difference, cache-header pass-through, no-local-storage enforcement test, timeouts, partial-failure handling, metrics, Dockerfile | All set operations pass integration tests; upstream outages degrade gracefully |

### Testing
- **Unit and server tests** (`go test ./...`): fakes of Jellyfin per package; the server
  tests run the whole stack (index, identity, catalog, proxy, sources) against two fake
  upstreams, including every set operation, failover, hung upstreams, and the
  no-local-storage invariant.
- **Rig** (`testrig/`, native, no containers): two real Jellyfin servers with synthetic
  media, behind a fault-injecting proxy. `smoke.py` drives JellyBloom like a client;
  `compare.py` diffs user-state behavior against real Jellyfin; `failover.py` injects
  faults; `setops.py` runs intersection and both differences against the servers' contents.
- **Container:** `Containerfile`, built and run with podman; CI builds it too.
- **Manual:** jellyfin-web first, then one mobile or TV client and one music client.

## Config additions

See `config.example.yaml` for the full file. Beyond upstreams and users:

```yaml
merge: difference         # union | intersection | difference
base: home                # difference only: A in A \ B
subtract: [friend]        # difference only: B (one or more upstreams)
web_upstream: home        # upstream serving /web (default: first upstream)
stall_timeout: 10s        # a relayed file this long without bytes fails over
upstream_timeout: 15s     # an upstream this long without answering an API request is skipped
metrics_listen: 127.0.0.1:9101   # Prometheus metrics (separate address; off by default)
log_format: json          # text (default) | json
log_level: info           # debug | info | warn | error
```
