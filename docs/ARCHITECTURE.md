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
- **Mapping table:** `unifiedId → [(upstream, realId), …]`.
- **Set operations** (`union`, `intersection`, `difference`) run over the match-key groups.
- **Metadata conflicts:** the preferred upstream (config order) wins, and the other
  upstreams fill any gaps.

### Views
`/UserViews` is synthesized by grouping upstream libraries by `CollectionType`. A music-only
upstream and a video-only upstream together produce Music, Movies, and Shows views.

### Active source
Each `(user, device, unifiedItem)` has an **active source**, the upstream that serves that
item:
- It is the top-ranked upstream by default (ranked by reachability, quality or
  direct-play fit, latency, then config order).
- `PlaybackInfo` sets it to the upstream of the `MediaSource` the client picked.
- Streams, HLS, subtitles, progress reports, media segments, and generic-fallback calls for
  that item all go to the active source. This keeps transcode sessions and plugin data, such
  as intro timestamps, matched to the file actually being played.

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
   `MediaSources` are merged and ranked. Each `MediaSource.Id` is replaced by a unified ID
   encoding `(upstream, realItemId, realMediaSourceId)`. `TranscodingUrl` is rewritten, and
   `PlaySessionId → upstream` is recorded.
2. **Direct play** (`/Videos/{id}/stream`, `/Audio/{id}/universal`): relayed with the
   `Range` header passed through.
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
| 3 | **Identity** | Match-key registry per type, UUIDv5 IDs, mapping table including lazy mappings, set-operation engine | Same item on two servers yields one ID; table tests pass for all set operations |
| 4 | **Server skeleton and generic routing** | Auth and token extraction, `/System/*`, `/socket`, case-insensitive router, **generic fallback router**, **response rewriter**, **`/web` proxy to `web_upstream`**, unhandled-route log | Stock jellyfin-web loads from JellyBloom and logs in; unknown routes reach the right upstream |
| 5 | **Browse (union only)** | `/UserViews` synthesis, `/Items` queries, item details, search, images, `Fields=` trimming | **Milestone:** a music-only and a video-only upstream browse as one server |
| 6 | **Playback** | `PlaybackInfo` merge, **active-source tracking**, direct-play relay, HLS rewriting, `PlaySessionId` routing, progress forwarding | Video and music play and seek; transcodes are cleaned up on stop |
| 7 | **User state** | Played, favorites, resume, next-up, playlists, collections | State persists across upstreams and devices |
| 8 | **Set operations and hardening** | Intersection and difference, failover, image cache, timeouts, partial-failure handling, metrics, Dockerfile | Killing an upstream mid-session degrades gracefully |

### Testing
- **Unit tests:** match keys, ID mapping, set operations, GUID and URL rewriter, HLS
  playlist rewriter.
- **Integration tests:** a docker-compose setup with a music-only Jellyfin, a video-only
  Jellyfin, and an overlapping-video Jellyfin. Includes one upstream running a plugin with
  an HTTP API, such as Intro Skipper, to exercise generic routing.
- **Manual:** jellyfin-web first, then one mobile or TV client and one music client.

## Config additions

```yaml
web_upstream: home        # upstream serving /web (default: first upstream)
upstreams:
  - name: friend
    url: https://jellyfin.example.com
    api_key: 0123456789abcdef
    user: guest           # required when using api_key
```
