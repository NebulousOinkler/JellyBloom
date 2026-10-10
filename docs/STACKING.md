# Stacking JellyBloom (plan, not started)

An outer JellyBloom that uses inner JellyBloom instances as upstreams. Set operations then
compose, e.g. (A ∪ B) \ C as an outer difference whose base is an inner union of A and B,
and households can sit under a group instance (each household's JellyBloom is an upstream
of the group's).

JellyBloom already presents itself as a Jellyfin server, so most of this works by design;
a read-only review (2026-10-10) found the gaps below. Status: deferred; nothing built yet.

## Gaps to close

1. **Pairing.** JellyBloom answers `/QuickConnect/Enabled` with `false`
   (`internal/server/server.go`), so an outer can pair with an inner only by `-password`
   or a `password_file`. Per-device upstream sessions (one per client device) also need a
   password then. Either document this, or support Quick Connect for JellyBloom's own users
   (initiate, approve as the user, connect).
2. **Change notifications.** `Server.Broadcast` is called only in tests, so JellyBloom never
   sends `LibraryChanged`. An outer would notice an inner's changes only at its 6-hour
   resync (`index.SyncerOptions.ResyncInterval`). Broadcast `LibraryChanged` after each
   catalog reload, ideally listing the unified IDs added, updated, and removed.
3. **Timeouts.** Both layers default to 15 s for API calls, 60 s for media, a 3 s
   PlaybackInfo grace, and a 10 s stall timeout (`internal/proxy`, `internal/playback`,
   `internal/sources`). An inner that is itself waiting on a slow upstream will make the
   outer give up first. Give the outer longer timeouts than the inner (configurable
   PlaybackInfo grace too), or document how to set them.
4. **Loops.** An instance listed as its own upstream, directly or through a chain, would
   recurse. Refuse an upstream whose server ID equals our own; the proxy already knows both
   (`Proxy.UpstreamIDs`, the auth service's `ServerID`).
5. **Redundancy.** Each inner looks like a single copy to the outer, so the outer's report
   undercounts copies (an item on both A and B behind one inner counts once). Note this in
   the docs; the inner's own report remains the source of truth for its upstreams.
6. **Test.** A rig test running JellyBloom over JellyBloom: an inner union of a and b, and an
   outer difference with that inner as base (and a subtract upstream). Browse, play, fail
   over (a fault under the inner, seen from the outer), and propagate a change from a server
   through both layers.

Also to verify while doing this: the outer's incremental sync asks an upstream for
`/Items/{id}/Ancestors`, which JellyBloom serves through its generic fallback today; and
the outer's upstream account on the inner should be an ordinary (non-admin) JellyBloom user.

When done: commit and push once the tests pass.
