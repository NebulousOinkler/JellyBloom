# Admin UI Scaffold (placeholder)

Status: placeholder / initial scaffold

Purpose
- Provide the pages, API endpoints, and minimal UX elements to enable non-CLI configuration, pairing, and operational visibility.

Pages / routes (MVP)
- /ui/login — admin login
- /ui/setup — initial setup wizard; add first admin user, basic config
- /ui/upstreams — list/add/edit upstream Jellyfin servers; show status, latency
- /ui/pair — pairing flow: show pairing codes, accept QuickConnect style approvals
- /ui/users — map upstream accounts to virtual users
- /ui/libraries — create and manage virtual libraries; assign upstream libraries
- /ui/playback — playback settings (proxy vs redirect/hybrid, stall_timeout)
- /ui/reports — view Report output, live upstream health
- /ui/metrics — simple Prometheus metrics link and examples

API endpoints (examples)
- GET/POST /api/upstreams
- POST /api/pair (exchange code)
- GET /api/report
- POST /api/playback-mode

Tech choices (suggested)
- Lightweight SPA (React/Vue) built into a static bundle; serve via the same binary or a small static server.
- Reuse any existing admin HTTP endpoints in JellyBloom; add a small management API that the UI calls.
- Authentication: reuse JellyBloom's existing user handling; support an admin user with session cookie or JWT.

Acceptance criteria
- An admin can add an upstream using the UI and see its health and latency.
- An admin can initiate the pairing flow from the UI and confirm a pairing using the existing pairing CLI flow or by entering the upstream quick-code.

Developer tasks (first cut)
- Add static UI build pipeline (npm build into /ui/dist and serve as embedded assets).
- Implement basic management API endpoints and wire them to the existing pairing logic.
- Add route /web that proxies/hosts the embedded Jellyfin web client (optional).

Integration points
- metrics, report, pairing CLI must be callable from the management API.

