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
| Version digests (partial content hashes) | Subtitle files |
| JellyBloom's own users, tokens, user data, playlists | Images and artwork |
| Source health and throughput stats | Transcode output |

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
  `ImageTags` already change whenever an image changes.
- **Enforcement:** a test runs the full integration suite with JellyBloom's data directory
  inspected afterwards. It may contain only the SQLite database and no files larger than
  the database's expected size. CI also fails on `os.Create`, `os.CreateTemp`, or
  `os.WriteFile` outside the store package.

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
  and re-fetches the affected IDs, with a periodic resync as a backstop.
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

### Redundant sources
A unified item usually has several copies across upstreams. JellyBloom keeps all of them
and uses them for three things: playing from the fastest copy, routing around broken
files, and keeping items available when an upstream is down.

**Versions vs. sources.** Copies are grouped in two levels:
- A **version** is a distinct encode. Two copies are the same encode when they match on
  container, codecs, resolution, bitrate, runtime, and file size, plus a partial content
  hash (first and last few MB, fetched lazily with Range requests and streamed through the
  hash; only the digest is stored).
- A **source** is one location of a version: `(upstream, realItemId, realMediaSourceId)`.

`PlaybackInfo` shows the client one `MediaSource` per **version**, so clients display
"1080p HEVC" and "4K HDR" as normal Jellyfin versions. Version names describe the encode
and never the upstream. Each version's unified `MediaSource.Id` maps to its ordered list of
sources.

**Source ranking** within a version is re-evaluated per play request:
1. Health. Quarantined sources are skipped.
2. Measured throughput and latency from JellyBloom to the upstream, as rolling stats from
   real relays plus light probes.
3. Config order.

Version choice, when the client doesn't pick one, follows the client's `DeviceProfile`:
direct-play-compatible first, then quality.

**Failover.**
- *Identical sources (same version):* failover is seamless at the byte level. If an
  upstream errors, stalls, or truncates mid-relay, JellyBloom reissues the same `Range`
  request to the next source and keeps writing to the client. The client sees no break.
  This works for direct play and direct stream, which are the common case.
- *Transcoded (HLS) playback:* JellyBloom starts a transcode on the next source at the
  current position and serves the following segments from it. The client may see a brief
  re-buffer.
- *Different versions:* JellyBloom never switches versions silently. If every source of the
  chosen version fails, playback errors, and the client can pick another version.

**Broken files.** A source is marked unhealthy for that item when it returns upstream 5xx
on media routes, truncated reads, ffmpeg or transcode failures, or a probe mismatch against
its version (runtime or stream layout). After N failures it is quarantined, then re-checked
on a backoff schedule. The other sources keep serving.

**Availability.** An item stays visible and playable while any source is reachable.
Upstream outages only reduce redundancy.

**Redundancy report** (admin-only JellyBloom endpoint and log): lists items with a single
source and quarantined sources. JellyBloom does not copy files itself; the report shows
what needs backing up.

**Limitation:** media bytes flow through JellyBloom, so "fastest source" means fastest to
JellyBloom. Regional benefit comes from placing JellyBloom near its clients, or running
one JellyBloom instance per region with the same config.

### Active source
Each `(user, device, unifiedItem)` has an **active source**, the source currently serving
that item:
- By default it is the top-ranked source of the top-ranked version.
- `PlaybackInfo` sets the version the client picked. The source is the best-ranked source
  within that version, and it can change on failover.
- Streams, HLS, subtitles, progress reports, media segments, and generic-fallback calls for
  that item all go to the active source. Failover only moves between identical files, so
  plugin data such as intro timestamps still matches the file being played.

### Rewriting (applies to every forwarded response)
- **Requests:** unified IDs → real IDs, and the client token → the upstream token.
- **Responses, text bodies only (JSON, m3u8, HTML, JS):**
  - Every GUID that is a known real ID on that upstream → its unified ID.
  - An unknown GUID gets a lazy mapping, `UUIDv5(upstream:id)`, recorded in the mapping
    table, so it can be routed later.
  - The upstream base URL and token are stripped.
- **Binary bodies** pass through untouched.

### Playback
1. `POST /Items/{id}/PlaybackInfo` goes to every upstream holding the item. Their
   `MediaSources` are grouped into versions and ranked. Each version gets a unified
   `MediaSource.Id` that maps to its source list. `TranscodingUrl` is rewritten, and
   `PlaySessionId → active source` is recorded.
2. **Direct play** (`/Videos/{id}/stream`, `/Audio/{id}/universal`): relayed with the
   `Range` header passed through, with byte-level failover across identical sources.
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
- **Unit tests:** match keys, ID mapping, set operations, GUID and URL rewriter, HLS
  playlist rewriter.
- **Integration tests:** a docker-compose setup with a music-only Jellyfin, a video-only
  Jellyfin, and an overlapping-video Jellyfin. Includes one upstream running a plugin with
  an HTTP API, such as Intro Skipper, to exercise generic routing.
- **Manual:** jellyfin-web first, then one mobile or TV client and one music client.

## Config additions

```yaml
merge: difference         # union | intersection | difference
base: home                # difference only: A in A \ B
subtract: [friend]        # difference only: B (one or more upstreams)
web_upstream: home        # upstream serving /web (default: first upstream)
upstreams:
  - name: friend
    url: https://jellyfin.example.com
    api_key: 0123456789abcdef
    user: guest           # required when using api_key
```
