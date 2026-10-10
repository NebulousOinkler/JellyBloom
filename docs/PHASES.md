# Build Phases

The tasks behind each phase in [ARCHITECTURE.md](ARCHITECTURE.md#build-plan). Each phase ends
with the "done when" check from that table. Target upstream version: Jellyfin 10.9+.

## Phase 1: Upstream client (`internal/upstream`)
- [x] `Client` per upstream: base URL, HTTP client with timeouts, a stable `DeviceId`
- [x] `MediaBrowser` authorization header (Client, Device, DeviceId, Version, Token)
- [x] Login with username/password (`POST /Users/AuthenticateByName`) → token, user ID
- [x] Login with an API key: resolve the configured `user` to a user ID with `GET /Users`
- [x] Request helper:
  - [x] re-login once on 401
  - [x] retry with exponential backoff on network errors, 5xx, and 429 (honor `Retry-After`)
- [x] `PublicInfo` (`GET /System/Info/Public`) and `Ping` (health check plus latency)
- [x] `Views` (`GET /UserViews`)
- [x] `Items` query (`GET /Items`): typed query params; items kept as raw DTO JSON
- [x] `EachItemPage`: pages through a recursive item listing
- [x] `Listen`: upstream `/socket` WebSocket
  - [x] keepalive (`ForceKeepAlive` / `KeepAlive`)
  - [x] `LibraryChanged` events on a channel
  - [x] reconnect with backoff
- [x] Tests against a fake Jellyfin (`httptest`) covering all of the above

## Phase 2: Index and sync (`internal/index`)
- [x] SQLite schema (`items`, `views`, `item_views`, `sync_state`), pure-Go driver, WAL
- [x] Full sync per upstream into a staging generation, then an atomic swap
- [x] Incremental sync from `LibraryChanged` (add, update, remove)
- [x] Periodic resync and a per-upstream sync status
- [x] Startup: serve the last good index while a resync runs
- Known gap: a music artist changed between full syncs is re-indexed without its library
  link until the next full sync (artists are outside the `/Items` tree)

## Phase 3: Identity (`internal/identity`)
- [x] Match-key registry per item type: movie, series, season, episode, artist, album, track, fallback
- [x] UUIDv5 unified IDs in Jellyfin's 32-hex format
- [x] Mapping table: unified ID ↔ every `(upstream, realId)`, plus lazy mappings
- [x] Hierarchy remap: `ParentId`, `SeriesId`, `SeasonId`, `AlbumId`, artists
- [x] Set-operation engine: `union`, `intersection`, `difference` (`base` \ `subtract`)
- [x] Config: `base` and `subtract` keys, validated
- [x] Table-driven tests for all set operations
- Matching: items carry several strong keys (provider IDs) and one weak key (normalized
  title/year, or parent + index). Shared strong keys always merge; weak keys merge only
  when no provider IDs conflict.
- Stability: every key remembers its unified ID, so IDs survive new copies, resyncs, and
  remove/re-add. Groups bridged by a new copy keep one ID and redirect the other.
- Set operations apply to leaves (movies, episodes, tracks…); containers are visible when
  any child is, so `difference` never hides an a-only episode behind a shared series.

## Phase 4: Server skeleton and generic routing (`internal/server`, `internal/rewrite`)
- [x] Case-insensitive router
- [x] Token extraction from all four places clients send it
- [x] JellyBloom users: `AuthenticateByName`, `/Users/Me`, QuickConnect stub
- [x] `/System/Info`, `/System/Info/Public`, branding, display preferences, stable `ServerId`
- [x] Client `/socket`: KeepAlive, plus `LibraryChanged` and `UserDataChanged` broadcasts
- [x] Rewriter: GUID ↔ unified in text bodies, upstream URL and token stripping, lazy mapping
- [x] Generic fallback router: extract IDs, pick the active source or primary upstream, forward, rewrite
- [x] `/web/*` proxy to `web_upstream`
- [x] Unhandled-route log
- [x] `jellybloom hash-password`; `users` and `fallback_write_allow` config
- Fallback safety: JellyBloom forwards with its own upstream credentials, so admin/system
  routes are always 403, writes need an allowlist entry (built-ins cover playback and user
  data), and only images/localization are anonymous.
- Users are never reported as administrators, so jellyfin-web hides the dashboard.
- Known gap (phase 6): every client reaches an upstream as the single JellyBloom device,
  so concurrent transcodes from different clients on one upstream may collide.
- Browse endpoints (`/UserViews`, `/Items`) currently fall back to the primary upstream
  only; phase 5 replaces them with merged answers from the index.

## Phase 5: Browse, union only (`internal/api`)
- [x] `/UserViews` synthesized by `CollectionType`
- [x] `/Items`: filters, `SortBy`, paging, `Recursive`, `IncludeItemTypes`, `searchTerm`
- [x] `/Items/{id}`, `/Items/Latest`, `/Items/Filters`, `/Search/Hints`
- [x] `/Artists`, `/Genres`, `/Studios`, `/Persons`, `/Shows/{id}/Seasons|Episodes`
- [x] Image relay (cache headers passed through)
- [x] `Fields=` trimming and a `UserData` overlay stub
- [x] **Milestone (against fake upstreams):** a music-only and a video-only upstream browse as one server
- [x] **Milestone (real, API level):** verified against two real Jellyfin 12.2 servers with `testrig/smoke.py`
- [ ] **Milestone (real, UI):** click through jellyfin-web at http://127.0.0.1:8200/web/
- Libraries merge by collection type + folded name ("Movies" on two servers → one; "Kids
  Movies" stays separate); libraries whose items are all hidden are omitted.
- Genres, studios, people, and libraries get derived IDs aliased to every upstream copy, so
  artwork routes to the first and responses from any upstream show the merged ID.
- Merged DTOs: preferred upstream (config order) wins; empty fields filled from other copies;
  ChildCount/RecursiveItemCount recomputed over the merged tree; upstream UserData replaced.
- Fallback list responses drop items hidden by the set operation.
- Benchmarks (50k copies): catalog reload ~0.5 s; 100-item page ~15 ms.

## Phase 6: Playback (`internal/playback`)
- [x] `PlaybackInfo`: fan out to every source, group copies into versions, rank, assign unified `MediaSource.Id`s
- [x] Active-source table per (user, device, item); `PlaySessionId` → source
- [x] Direct-play and direct-stream relay with `Range` (bounded buffer, no disk)
- [x] HLS master and media playlist rewriting; segment routing
- [x] Subtitle relay
- [x] Forward progress reports; clean up on `Stopped` and `ActiveEncodings`
- [x] Per-client upstream devices: each (JellyBloom user, client device) is its own device on
  every upstream (own login for password upstreams; header DeviceId for API keys), so
  sessions and transcodes of different clients never collide. Client `DeviceId` query
  values map to it and back.
- Routing reuses the generic proxy: version IDs and JellyBloom play-session IDs are aliases
  over the upstream sources they stand for (best first), so playlists, segments, subtitles,
  progress reports, and `ActiveEncodings` reach the right upstream with its real IDs.
- Versions group copies by container, size, runtime, and stream layout; each is labelled by
  its encode ("4K HEVC HDR"), never by upstream. A single version keeps the item ID.
- Source order within a version is config order for now; phase 8 ranks by health and
  throughput and adds failover.
- Follow-ups (done in phase 9): prune old play-session aliases; hide upstream file `Path`
  in media sources.

## Findings from the real rig (Jellyfin 12.2)
- Library contents sit under a physical root `Folder`; it is now transparent, as in Jellyfin.
- Jellyfin copies the request's `ApiKey` into generated URLs (HLS variants), so the proxy
  swaps token parameters for the upstream token instead of stripping them.
- Artwork/metadata-only changes send no `LibraryChanged`; a finished scan
  (`RefreshProgress` 100 for a library root) now triggers a full resync.
- `LibraryChanged` arrives ~30 s after a change. Library IDs are derived from the library
  name, so same-named libraries share IDs across servers; item IDs differ. URLs use dashed
  GUIDs.

## Fixes after phase 7 (checked against real Jellyfin 12.2)
`testrig/compare.py` replays user-state steps on a real Jellyfin server and on JellyBloom
and diffs the results; it found the first group, the rig the rest.
- Playback rules now match Jellyfin exactly: progress reports apply the resume rules too; a
  start counts a play without moving the position (and dates it only if not yet played);
  music is played on start and never resumable; audiobooks use 5-minute rules; a stop
  without a position is a full play; the play is dated only when a resume point is kept or
  the item completes.
- Mark played with a date counts a play on that date, without one keeps the existing
  date; mark unplayed clears play count and last played. A rewatch (played, with a resume
  point) shows in Continue Watching. Folders always carry `PlayedPercentage`.
- Next Up anchors on the furthest played episode (not the most recent), offers a series'
  first episode once any episode is started, orders series by their latest activity,
  drops a series whose next episode is in progress when `enableResumable=false`, and
  supports `enableRewatching` and `nextUpDateCutoff`. `disableFirstEpisode` is ignored, as
  in Jellyfin 12.2.
- Incremental sync put every changed item into every library of its upstream (Jellyfin
  ignores `ParentId` with `Ids`; the unit-test fake did not). Libraries now come from
  `/Items/{id}/Ancestors`, and libraries added upstream appear within seconds of
  Jellyfin's change notice instead of at the next periodic resync.
- Upstream collections showed empty (Jellyfin links BoxSet members); they now list their
  members, and adding to or removing from them works (kept in JellyBloom).
- Concurrent user-data and playlist edits failed with "database is locked" or lost
  updates; they now take the write lock up front.
- Playlist items honor `SortBy` (shuffle plays shuffled); playlist and collection routes
  accept dashed GUIDs; playlists and collections can be favorited and marked played.
- Rig: a 10-minute movie byte-identical on both servers, more episodes (two seasons), a
  long track, an audiobook library, and a collection on server a.

## Phase 6.5: Upstream credentials (`internal/secrets`, `internal/pairing`, `internal/redact`)
- [x] Config holds no upstream secrets; strict decoding rejects removed/unknown fields
- [x] `jellybloom pair` via Quick Connect (default), `-password` (exchanged once), `-api-key` (opt-in);
  `jellybloom unpair` revokes and deletes; re-pairing revokes the replaced token
- [x] Secret store: owner-only files (startup refuses loose permissions) or OS keychain;
  `token_command` and `password_file` sources
- [x] Admin accounts and API keys refused unless `allow_admin`, at pairing and every login
- [x] Per-client upstream devices minted through Quick Connect; fallbacks with warning
- [x] Log redaction for every line; JellyBloom session tokens stored hashed
- [x] Verified on the rig: pairing as a non-admin account, upstream-side revocation, a clear
  "re-pair" error with back-off, and live recovery after re-pairing
- Later (phase 9 or beyond): per-person pairing, so each JellyBloom user's own upstream
  account and permissions apply

## Phase 7: User state (`internal/userdata`)
- [x] Played, favorite, resume position, play count; both route styles
- [x] `/Items/Resume`, `/Shows/NextUp`
- [x] Playlists and collections over unified IDs
- [x] Per-user `UserData` on every item; folders (series, seasons, albums) get
  `UnplayedItemCount` and played state over their playable items
- [x] Played/favorite/likes filters and `DatePlayed`/`PlayCount`/`IsFavoriteOrLiked`/`IsPlayed` sorts;
  Latest hides played items (`HidePlayedInLatest`)
- [x] Playback reports apply Jellyfin's resume rules (5% / 90% / 300 s) locally and are still
  forwarded upstream (ending upstream transcodes)
- [x] `UserDataChanged` pushed to the user's own sockets
- [x] User data, playlist entries, and collection items follow identity redirects when items merge
- [x] Playlists are private and ordered (duplicates allowed); collections are shared and appear
  alongside upstream BoxSets; `DELETE /Items/{id}` works only for JellyBloom's own lists
- [x] Verified on the rig (`smoke.py`): resume, watched at 95%, Next Up, series counts, favorites,
  playlists
- Not done: importing existing watch history from upstream accounts; playlist sharing

## Phase 8: Redundancy (`internal/sources`)
Decisions (2026-10-10): sampled-block fingerprints plus block hashes taken during playback
(no full background hashing); speed measured passively from playback plus latency pings (no
periodic range-read probes); the redundancy report is an admin-only endpoint plus a CLI; a
broken copy that is the only one available is still served, and reported.

- [x] Rig: `testrig/faultproxy` in front of both servers (JellyBloom reaches a via :8211 and
  b via :8212): outage, latency, throttling, bodies that break off or stall, error statuses
  on media; `servers.sh faults a|b JSON|clear`
- [x] Source registry: fingerprints, verdicts, quarantines, and block hashes in SQLite;
  speed stats and play sessions in memory
- [x] Content identity: 17 sampled 64 KiB blocks (first, last, 15 between, one at the middle),
  streamed through SHA-256, 8-byte digests stored; small files whole. Versions split when
  fingerprints differ; truncated copies (a prefix of a longer copy) and damaged copies
  (a zeroed block where the peer has data, or the minority of three or more) are left out
  when a good copy is available; disputed pairs become separate versions
- [x] Block hashes during playback: 1 MiB blocks of copies with identical peers; a block
  that disagrees is re-read from every copy and settled, and a relay on the damaged copy
  moves to a good one
- [x] Background verifier: every multi-copy item, paced; resamples fingerprints after 30
  days; re-reads damaged evidence and re-decides disputes daily; forgets copies that are gone
- [x] Throughput (relay windows where the upstream was the bottleneck) and latency (pings
  every 30 s, response times) per upstream
- [x] Ranking: health, then speed (25% margin to pass an earlier upstream; unmeasured counts
  as fastest, so each is measured once), then config order; applied per PlaybackInfo and
  per relay
- [x] Active upstream per play session (by `PlaySessionId`, or device and item): streams,
  HLS, reports, and fallback calls follow it, and failovers move it
- [x] Byte-level failover for files as stored (`static=true` streams, downloads): error or
  stall before responding, or a break-off, stall, or damage mid-body, continues from an
  identical copy with `Range: bytes=pos-last`; per-upstream `ETag`/`Last-Modified` dropped
- [x] Retry for everything else (HLS, remux, subtitles, reports, plugin routes): an
  unreachable upstream, or a 5xx to a GET, before anything was sent is retried on the next
  upstream holding the items. HLS continues on the next upstream's transcode. IDs minted for
  one upstream's opaque values (the `Tag` in segment URLs) never block a retry
- [x] Health: three failures in 10 minutes quarantine a source (1 min, doubling to 1 h, with
  one-byte re-checks; persisted); a success ends it. Connection failures mark the upstream
  unreachable until a request or ping succeeds. PlaybackInfo skips unreachable upstreams and
  waits at most 3 s for slower ones after the first answer
- [x] Redundancy report: `GET /JellyBloom/Report` (users with `admin: true`) and
  `jellybloom report [-json] [-all]`: single-copy items, broken, disputed, and quarantined
  copies with file paths, unchecked multi-copy items, live upstream state
- [x] Verified on the rig (`testrig/failover.py`, 28 checks): the verifier finds b's damaged
  and truncated copies; each movie is one version; direct play survives the active upstream
  breaking off at 30 MB, stalling at 20 MB, or answering 500, byte-exact; repeated failures
  quarantine a copy, and serving it as the only copy ends that; the damaged copy is served
  while a is down; HLS segments continue from the other upstream's transcode when the
  transcoding upstream goes down; a throttled upstream loses plays to the faster one;
  PlaybackInfo answers in under 5 s when b takes 8 s. `smoke.py` and `compare.py` still pass
- Limits: sampling catches damage only where a sample lands; damage elsewhere is found once
  both copies have been played over it (the first such play may show a glitch). Remuxed
  streams and `/Audio/{id}/universal` can only fail over before they start. Speed is
  measured to JellyBloom, not to the client

## Phase 9: Set operations and hardening
Decisions (2026-10-10): install podman on the dev machine to build and run the image;
metrics on a separate address, off by default; a GitHub Actions workflow; neutral file
paths for clients.

- [x] Intersection and difference end to end: `TestSetOperations` drives union,
  intersection, a \ b, and b \ a through the client API (libraries, movies, Latest,
  episodes, genres, music, series counts, artwork routing, lookups, PlaybackInfo sources,
  favorites, search); mutation-checked against the set-operation engine. On the rig,
  `testrig/setops.py` runs three extra instances and checks each against the servers' own
  contents (14 checks)
- [x] Partial failure: per-request upstream timeouts (API `upstream_timeout` 15 s, media
  60 s, known-down upstream 5 s), retry on the next upstream after a timeout, device
  session minting bounded to 10 s with a fallback to JellyBloom's session. With a hung
  server on the rig, requests only it could answer took 80–90 s before and now take 25 s
  the first time and 5 s after; browsing stays instant (`TestHungUpstream`, `failover.py`)
- [x] No-local-storage enforcement: `internal/storagecheck` (source scan: no file writes or
  multipart spooling outside the secret store) and `TestNoLocalStorage` (plays media with
  `TMPDIR` redirected; only the SQLite files may exist, far smaller than the media);
  both mutation-checked
- [x] Metrics (`metrics_listen`, Prometheus text format, dependency-free
  `internal/metrics`), per-request debug logging with templated paths and no query
  strings, `log_format: json`, `log_level`
- [x] `Containerfile` (static binary, distroless, non-root, 16 MB) built and run with
  podman against the rig: browsing, PlaybackInfo, and byte-exact direct play from the
  container; `/data` held only the database and the paired tokens
- [x] CI (`.github/workflows/ci.yml`): gofmt, vet, race tests (including the storage
  checks), static Linux binaries for amd64 and arm64, and the container image
- [x] Neutral file paths for clients (`/media/` plus the last parts); the admin report
  keeps the real ones
- [x] Play session aliases are marked and pruned after 7 days; minted IDs (opaque upstream
  GUIDs) only prefer their upstream when routing
- [x] Cache headers pass through for artwork and other forwarded bodies (since phase 4);
  relayed files drop per-upstream validators (phase 8)
- Superseded: the docker-compose test rig (the native rig plus podman covers it)
- Later: hot reload or an admin API for upstreams; per-person pairing; importing upstream
  watch history; playlist sharing

## After phase 9: the traveling library
- [x] `scripts/travel.sh`: library (union, laptop first, laptop serves the web app),
  packing list (home \ laptop), unpacking list (laptop \ home); `init`, `start`, `stop`,
  `status`, `sync`, `report`
- [x] User state sync between instances: change times on records and playlists, playlist
  deletion records, export and merge (most recent change wins), items matched by match keys;
  admin endpoints and `jellybloom sync`
- [x] `testrig/travel.py` (19 checks) runs the whole scenario on the rig

## Planned: the Commons
A group-owned, self-healing archive for every kind of media (films, shows, music,
audiobooks, books, comics, documents): copy counting against a replication target, adoption
and surplus plans, repairs by vote. Not started; the plan, phases C0 to C8, and the
decisions to make first are in [COMMONS.md](COMMONS.md).

## Planned: stacking
An outer JellyBloom over inner JellyBloom instances, so set operations compose and
households can sit under a group instance. Deferred; the gaps to close and the test to add
are in [STACKING.md](STACKING.md).
