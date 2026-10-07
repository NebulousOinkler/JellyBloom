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
- [ ] Match-key registry per item type: movie, series, season, episode, artist, album, track, fallback
- [ ] UUIDv5 unified IDs in Jellyfin's 32-hex format
- [ ] Mapping table: unified ID ↔ every `(upstream, realId)`, plus lazy mappings
- [ ] Hierarchy remap: `ParentId`, `SeriesId`, `SeasonId`, `AlbumId`, artists
- [ ] Set-operation engine: `union`, `intersection`, `difference` (`base` \ `subtract`)
- [ ] Config: `base` and `subtract` keys, validated
- [ ] Table-driven tests for all set operations

## Phase 4: Server skeleton and generic routing (`internal/server`, `internal/rewrite`)
- [ ] Case-insensitive router
- [ ] Token extraction from all four places clients send it
- [ ] JellyBloom users: `AuthenticateByName`, `/Users/Me`, QuickConnect stub
- [ ] `/System/Info`, `/System/Info/Public`, branding, display preferences, stable `ServerId`
- [ ] Client `/socket`: KeepAlive, plus `LibraryChanged` and `UserDataChanged` broadcasts
- [ ] Rewriter: GUID ↔ unified in text bodies, upstream URL and token stripping, lazy mapping
- [ ] Generic fallback router: extract IDs, pick the active source or primary upstream, forward, rewrite
- [ ] `/web/*` proxy to `web_upstream`
- [ ] Unhandled-route log

## Phase 5: Browse, union only (`internal/api`)
- [ ] `/UserViews` synthesized by `CollectionType`
- [ ] `/Items`: filters, `SortBy`, paging, `Recursive`, `IncludeItemTypes`, `searchTerm`
- [ ] `/Items/{id}`, `/Items/Latest`, `/Items/Filters`, `/Search/Hints`
- [ ] `/Artists`, `/Genres`, `/Studios`, `/Persons`, `/Shows/{id}/Seasons|Episodes`
- [ ] Image relay (cache headers passed through)
- [ ] `Fields=` trimming and a `UserData` overlay stub
- [ ] **Milestone:** a music-only and a video-only upstream browse as one server in jellyfin-web

## Phase 6: Playback (`internal/playback`)
- [ ] `PlaybackInfo`: fan out to every source, group copies into versions, rank, assign unified `MediaSource.Id`s
- [ ] Active-source table per (user, device, item); `PlaySessionId` → source
- [ ] Direct-play and direct-stream relay with `Range` (bounded buffer, no disk)
- [ ] HLS master and media playlist rewriting; segment routing
- [ ] Subtitle relay
- [ ] Forward progress reports; clean up on `Stopped` and `ActiveEncodings`

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
