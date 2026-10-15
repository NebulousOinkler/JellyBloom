# Design: Direct-playback with Proxy Fallback

Status: placeholder / initial design

Goals
- Reduce JellyBloom CPU and bandwidth usage for large media streams by enabling direct-client streaming from upstream servers when safe.
- Preserve JellyBloom's resilience features: the ability to prefer fastest copies, failover mid-stream or on error, and collect consistent metrics.

High-level flow options

1) Redirect-first (lightweight)
- On playback request, JellyBloom authenticates the client and obtains an upstream URL (signed, short-lived) or issues an HTTP 302 redirect to the chosen upstream.
- Advantages: minimal proxy bandwidth; client streams directly from upstream.
- Drawbacks: losing mid-stream transparent failover unless client reconnect logic is used.

2) Hybrid (recommended)
- Start with a short-lived redirect/direct URL so the initial data flows directly.
- Establish a proxy fallback URL in the metadata (or a separate control channel). If the upstream fails, the client can be instructed (via websocket or re-API) to retry with the proxied URL or JellyBloom can take over by starting to proxy the stream and signaling the client to reconnect.
- Advantages: balances performance and resilience.

3) Always-proxy (current model)
- JellyBloom proxies all data. Best resilience but highest resource usage.

Security considerations
- Upstream auth: generate signed URLs or temporary tokens for direct access; limit scope, path, and lifetime.
- Client trust: ensure client cannot request an arbitrary upstream; authorization must be validated per virtual user.
- Metrics: ensure metrics still capture relay bytes and failovers even when direct streams are used (instrument redirection counts and post-failure proxied bytes).

API changes / endpoints
- POST /Playback/Start (existing): return {strategy: "redirect"|"proxy", url: <signed url>, fallback_proxy_url: <url>}
- GET /health/redirects: metrics about redirects issued, failures, and fallbacks.

Fallback mechanics
- Failure detection: monitor upstream via L7 errors or stall timers. If a direct stream fails, detect client reconnect or provide a control websocket to notify clients.
- Client fallback patterns:
  - Client reconnects automatically to JellyBloom when the upstream returns error: JellyBloom accepts the new request and proxies content.
  - Alternatively, JellyBloom instructs the client via websocket to switch to fallback URL (if client supports it).

Acceptance criteria
- Admin can toggle global or per-upstream "playback_strategy": [proxy|redirect|hybrid].
- For hybrid mode, typical playback uses direct URL and on simulated upstream failure the stream can continue by proxying the content without losing the item in the user's client (or with an automated reconnect within 10s).
- Metrics include redirect counts, proxy bytes served, and number of fallbacks.

Open questions
- Which Jellyfin clients gracefully handle 302s vs explicit proxied streams? Need compatibility testing.
- How to instruct clients to switch mid-playback reliably — is websocket support adequate?

Implementation notes
- Add config options and a migration for existing installs.
- Expose admin metrics and a UI toggle (see admin UI scaffold).
- Start with redirect-first as an opt-in POC, then roll hybrid as the default.

