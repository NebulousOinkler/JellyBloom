# Expanded Design: Direct-playback with Proxy Fallback

Status: design spec (expanded)

Overview

This document expands the initial DIRECT_PLAYBACK design into a concrete engineering specification for implementing a "direct-playback with proxy fallback" strategy in JellyBloom. The goal is to reduce proxy bandwidth/CPU for large media while preserving JellyBloom's resilience (copy selection and mid-stream failover) and observability.

Goals
- Offer configurable playback strategies per deployment, per-upstream, and per-library:
  - proxy: current behavior (media proxied through JellyBloom)
  - redirect: return signed upstream URL / 302 so clients stream directly
  - hybrid: prefer redirect/direct URL but provide a proxied fallback if the upstream fails
- Minimize changes to clients where possible; avoid requiring client-side modifications unless optional websocket-based control is desired.
- Ensure security of direct access (signed temporary URLs, limited scope).
- Preserve metrics and operational visibility across all modes.
- Provide clear admin controls and migration path.

Constraints & assumptions
- Many Jellyfin clients accept 302 redirects for media resources; some clients may not. Compatibility testing is required (see Testing section).
- JellyBloom currently has pairing logic and upstream credentials; it can mint temporary credentials or signed URLs for upstream access.
- Some upstream servers may be behind NAT or otherwise unreachable directly from the client's network; the redirect flow must gracefully fall back to proxy in such cases.

Design options (expanded)

1) Redirect-first (stateless, simplest)
- On a client playback request for an item, JellyBloom:
  1. Authenticates the client and resolves which upstream copy should serve the request (ranking by latency/health/config).
  2. Requests a short-lived, signed URL / temporary token from that upstream or crafts a signed proxy-preserving URL if upstream supports it.
  3. Returns an HTTP 302 redirect (or a playback response containing {strategy: "redirect", url: ...}) to the client.
- Pros: very low bandwidth on JellyBloom; minimal server resource use.
- Cons: no transparent mid-stream takeover. If the upstream fails, the client must reconnect; JellyBloom can either record the failure (metrics) and instruct the client to reconnect to an alternate URL via application-layer APIs or websocket (if supported).

2) Hybrid (recommended default)
- Same beginning as redirect-first. Additionally:
  - JellyBloom returns a secondary proxied URL (or a token to request it) in the playback response metadata, plus an identifier for the chosen upstream and copy.
  - JellyBloom starts a short-lived background probe to validate the upstream connectivity and prepare to proxy quickly (optional optimization).
  - If JellyBloom detects the upstream failing (via health metrics, stall detection, or explicit L7 error), it marks the stream as failed and either:
    a) Signals the client to reconnect to the fallback proxied URL via a control websocket or server-sent event (if the client supports), or
    b) Waits for the client to retry; on retry JellyBloom serves the proxied URL so playback can continue.
- Pros: most resilience preserved, large data mostly bypasses the proxy.
- Cons: requires client behavior or an acceptance that some short downtime occurs while the client reconnects. Optionally requires websocket support for better UX.

3) Proxy-first (current)
- Keep existing behavior where JellyBloom proxies all media data. Keep as a supported option for deployments that prioritize resilience/inspection over bandwidth.

API & protocol changes

Playback response model
- Standardize a playback response that includes fields for strategy and fallback. Example (JSON conceptual):

  {
    "strategy": "hybrid",            // "proxy" | "redirect" | "hybrid"
    "primary_url": "https://upstream.example/stream?signed=...",
    "primary_is_redirect": true,      // whether client should 302 or directly use URL
    "fallback_proxy_url": "https://jb.example/stream/proxied/abcd",
    "copy_id": "upstream-uuid-1234",
    "expires_at": "2026-10-15T22:00:00Z"
  }

- The existing Jellyfin API surface must be preserved: many clients expect a direct HTTP response with the media body or a 302. If returning JSON breaks clients, prefer returning a standard HTTP 302 for primary redirect and include the fallback URL in a small HTTP header (e.g., X-JellyBloom-Fallback) or in the user's session metadata accessible via the control API.

New endpoints
- POST /api/admin/playback-mode (admin) — set global default and per-upstream/per-library preferences.
- GET /api/playback/prepare?item_id=... — internal endpoint used by UI/tools to inspect what a playback would return (strategy, urls). Not required for client compatibility but useful for diagnostics.
- Optional websocket endpoint: /api/control/ws — control channel to send live failover notifications to clients that have authenticated and registered a playback session.

Config model
- Global option: playback.default = proxy|redirect|hybrid
- Upstream override: upstreams[].playback = (null | proxy | redirect | hybrid)
- Library override: libraries[].playback = (null | proxy | redirect | hybrid)
- TTL options: signed_url_ttl_seconds, fallback_ready_timeout_seconds

Security model
- Signed URLs:
  - Use HMAC or upstream-provided temporary tokens to issue URLs that are valid only for the requested path, limited time, and optionally restricted by client IP (if upstream supports).
  - Avoid embedding upstream credentials in client-facing responses.
- Token minting:
  - If upstream supports token exchange, the proxy can request a temporary token on behalf of the client and forward it.
- Fallback protection:
  - Ensure fallback proxied URL requires the same session auth; proxied URLs should not be usable by third parties.

Metrics & observability
- Track and export the following metrics (Prometheus):
  - jellybloom_playback_requests_total{strategy=proxy|redirect|hybrid}
  - jellybloom_redirects_total
  - jellybloom_fallbacks_total
  - jellybloom_proxy_bytes_total (including proxied bytes served as fallback)
  - jellybloom_redirect_failures_total (client could not reach upstream or bad response)
  - jellybloom_fallback_latency_seconds_bucket (time between primary failure detection and fallback success)
- Logs: emit structured events on redirect issuance, fallback triggers, and successful mid-stream failovers.
- Admin UI: show counts and recent fallback events, allow filtering by upstream and time.

Failure detection & fallback mechanics (detailed)

Failure signals
- Health monitors: per-upstream active health checks and latency tracking (already present in JellyBloom). If health crosses thresholds, JellyBloom avoids selecting that upstream for new plays.
- Runtime errors: while a client is streaming directly, JellyBloom can still observe responses from the upstream (if proxying small control requests) or rely on metrics from the client path (e.g., client reports errors via control API or websocket).
- Stall detection on proxied streams: if JellyBloom is proxying, it uses stall_timeout to decide when to attempt failover.

Fallback initiation
- If the proxy detects an upstream failure during proxying, it can immediately take over the stream (current behavior) and continue serving bytes to the client.
- For redirected clients, fallback requires a client reconnect since the original TCP connection is between client and upstream. Options:
  - Passive: mark redirect failure in metrics and rely on client retry; on retry, the proxied URL is provided.
  - Active (better UX but client-dep): control websocket is used to request the client to reopen the playback to the fallback URL. The client must support this control channel. If not supported, accept a small interruption on failure and then serve proxied content on retry.

Seamless mid-stream takeover (advanced)
- For minimal interruption, consider application-layer techniques:
  - Range-based restart: when a direct stream fails at byte offset N, JellyBloom can start proxying the same content from byte N and provide a proxied URL that contains a start offset so the client can resume (if the client supports byte-range resume). This decreases rebuffer time.
  - Client must support byte-range requests or HTTP Range header for resume to work.

Client compatibility
- Clients that accept 302 or explicit direct URLs will benefit immediately.
- Clients that do not accept redirects will continue to work in proxy mode.
- Web UI (browser) clients generally respect redirects and will load media from upstream; mobile apps differ. Testing matrix is required.

Testing plan

Compatibility
- Test with a representative set of Jellyfin clients (web, Android, iOS, TV apps) for:
  - 302 redirect handling for media and HLS streams
  - Handling of range requests and resume after failure
  - Behavior when primary URL is unreachable

Functional tests
- Unit tests for signed URL generation, TTL, and per-upstream overrides.
- Integration tests (lightweight dev stack):
  - Start JellyBloom with two upstreams containing same media; verify hybrid mode returns redirect, then simulate upstream failure and verify fallback via reconnect or websocket-driven switch.
  - Test range-resume flow: start streaming to offset X, simulate cut, then reconnect with Range header and continue from correct offset.

Performance tests
- Measure CPU, memory, and network bandwidth on the JellyBloom host for proxy vs redirect vs hybrid under typical workloads (1–100 concurrent streams). Record failover latency and interruption duration.

Migration & rollout
- Add feature behind config flags and admin UI toggle.
- Start with opt-in hybrid/redirect mode: default remains proxy to avoid surprises.
- Provide clear release notes and migration docs that explain limitations and how to fallback to proxy-only mode if client incompatibility is observed.

Implementation plan (milestones)

M0 — Prep & API scaffolding (1 week)
- Add config model for playback strategies.
- Add admin API endpoints to read/set playback preferences.
- Add small diagnostic endpoint GET /api/playback/prepare to show what would be returned for an item.

M1 — Redirect-first POC (1–2 weeks)
- Implement signed URL minting and return HTTP 302 for playback requests when strategy=redirect.
- Emit metrics for redirects and redirect failures.
- Add tests for basic redirect flow.

M2 — Hybrid flow and fallback (2–4 weeks)
- Implement fallback URL generation and provide it to clients via header/metadata or optional JSON response when safe.
- Implement optional websocket control channel for live failover notifications.
- Implement metrics and admin UI hooks for fallbacks.
- Add integration tests simulating upstream failure.

M3 — UX and compatibility polish (2–4 weeks)
- Add admin UI controls to set defaults and override per-upstream.
- Add Grafana dashboard for metrics.
- Run broader client compatibility tests and document behavior.

Estimated total effort: 6–12 weeks of engineering effort (depending on team size and test coverage). POC can be done in 2–3 weeks.

Acceptance criteria (concrete)
- Admin can set playback.default and upstream-level override and the system honors these settings.
- Redirect-first mode returns a redirect or direct URL and reduces proxy bandwidth for steady-state streaming.
- Hybrid mode issues direct URLs and provides a proxied fallback; on simulated upstream failure, clients either automatically reconnect to continue playback (where supported) or can be signaled via websocket to switch to fallback. In either case the number of lost-playback-seconds during failover is minimized and measured.
- Metrics are reported for redirects, fallbacks, and proxied bytes.
- Documentation added: config reference, admin UI docs, and compatibility notes.

Risks & mitigations
- Client incompatibility: mitigate by keeping proxy as default and by documenting compatibility. Provide per-upstream override.
- Security misconfiguration (long-lived URLs): mitigate via short TTLs and HMAC-signed URLs.
- Increased complexity: keep hybrid and redirect features behind flags and roll them out gradually.

Open questions
- Should JellyBloom attempt to negotiate a direct TLS passthrough vs signed URL model for upstreams that support proxying via authenticated URL? (Depends on upstream capabilities.)
- Which clients should be our initial compatibility targets? (web is easiest, then Android, then others.)
- Do we want to add a client-side SDK for websocket control to reduce fragmentation of client behavior?

Appendix: Example header-based fallback approach
- When returning 302 for primary, include header: `X-JellyBloom-Fallback: https://jb.example/stream/fallback/abcd`.
- Clients that ignore the header still get redirected and stream directly; clients that understand the header can prefetch or register the fallback URL for quick reconnect.

References
- Repository: https://github.com/NebulousOinkler/JellyBloom
- Related concept: Jellyswarrm's direct-playback / minimal-overhead proxying goals and its sessions/WebSocket coordination work.

