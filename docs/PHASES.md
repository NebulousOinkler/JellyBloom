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
- Follow-ups for phase 9: prune old play-session aliases; consider hiding upstream file
  `Path` in media sources.

## Findings from the real rig (Jellyfin 12.2)
- Library contents sit under a physical root `Folder`; it is now transparent, as in Jellyfin.
- Jellyfin copies the request's `ApiKey` into generated URLs (HLS variants), so the proxy
  swaps token parameters for the upstream token instead of stripping them.
- Artwork/metadata-only changes send no `LibraryChanged`; a finished scan
  (`RefreshProgress` 100 for a library root) now triggers a full resync.
- `LibraryChanged` arrives ~30 s after a change. Library IDs are derived from the library
  name, so same-named libraries share IDs across servers; item IDs differ. URLs use dashed
  GUIDs.

## Phase 7: User state (`internal/userdata`)
- [ ] Played, favorite, resume position, play count; both route styles
- [ ] `/Items/Resume`, `/Shows/NextUp`
- [ ] Playlists and collections over unified IDs

## Phase 8: Redundancy (`internal/sources`)
- [ ] Partial content hashing, streamed, storing only the digest
- [ ] Throughput and latency stats per upstream from relays and probes
- [ ] Byte-level failover across identical sources mid-relay
- [ ] HLS failover: restart the transcode on the next source at the current position
- [ ] Health tracking, quarantine, and backoff re-checks
- [ ] Redundancy report endpoint (admin)

## Phase 9: Set operations and hardening
- [ ] Integration tests for intersection and difference
- [ ] Partial-failure handling and per-upstream timeouts in fan-out
- [ ] No-local-storage enforcement test and CI check for file-writing calls
- [ ] Metrics, structured logging, Dockerfile, docker-compose test rig
