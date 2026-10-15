# Prioritized Issues for JellyBloom

This document lists proposed issues prioritized to satisfy common Jellyswarrm users and improve adoption.

How to use
- Each entry below is written as a short issue summary + suggested description, priority, estimated effort, and suggested labels.
- I recommend creating each as a GitHub Issue with the title line and copying the body below. The two design items have dedicated placeholder docs linked below.

Priority key
- P0: High impact, should be done soon.
- P1: Medium impact, next wave.
- P2: Nice-to-have / long-term.

Suggested labels: enhancement, ux, infra, metrics, docs, design, good-first-issue, help-wanted

---

1) Web admin UI (Setup wizard + Basic management)
- Priority: P0
- Effort: Medium
- Description: Provide a web-based admin UI that exposes pairing/setup, upstream management, user mappings, and a page to view reports and metrics. Include a minimal bundled Jellyfin web client route at /web for convenience.
- Acceptance: Admins can add upstreams, pair (using existing pairing CLI flow), create virtual libraries and view the Report page without touching CLI.
- Suggested labels: enhancement, ux, good-first-issue

2) Docker images + Compose examples + Releases
- Priority: P0
- Effort: Low
- Description: Publish CI-built container images (GHCR) and add a docker-compose example for common deploys. Add release workflow and changelog.
- Acceptance: Users can run a published image with a single docker-compose file and follow release notes to download binaries/images.
- Suggested labels: infra, docs

3) Direct-playback mode with proxy fallback (design placeholder)
- Priority: P0
- Effort: Medium
- Description: Add an option to return direct upstream URLs (or 302 redirect) for large media streams to avoid proxy bandwidth/CPU; on failure the proxy should be able to transparently take over streaming to enable failover.
- Acceptance: Admins can toggle "performance-first" mode; under normal conditions streams do not flow through JellyBloom; on upstream failure JellyBloom can proxy mid-stream or cause the client to reconnect to a proxied endpoint.
- Suggested labels: enhancement, design
- See: docs/DESIGN_DIRECT_PLAYBACK.md

4) User mapping UI & QuickConnect pairing flow
- Priority: P0
- Effort: Medium
- Description: Implement QuickConnect-like code approval flow (pairing from upstream device) and a UI to map upstream accounts to a virtual user, with easy syncing of watch history and playlists.
- Acceptance: Users can map accounts in the UI, and history/playlist sync runs with conflict resolution.
- Suggested labels: enhancement, ux

5) Health endpoints & Docker healthchecks
- Priority: P1
- Effort: Low
- Description: Publish /health and /ready endpoints, add examples for Docker healthchecks, and document minimal probe settings.
- Acceptance: Kubernetes / Docker healthchecks can reliably detect readiness and liveness.
- Suggested labels: infra, docs

6) Prometheus metrics + Grafana dashboard example
- Priority: P1
- Effort: Low–Medium
- Description: Expand metrics (upstream latency, active streams, failovers, relay bytes) and provide a Grafana dashboard JSON for common monitoring.
- Acceptance: Operators can import the dashboard and observe failovers and throughput.
- Suggested labels: metrics, infra

7) Embedded/optional Jellyfin Web UI route
- Priority: P1
- Effort: Medium
- Description: Serve or proxy a bundled Jellyfin web client at /web so users get a familiar client without extra setup.
- Acceptance: The route serves a working Jellyfin web UI that talks to JellyBloom as a Jellyfin server.
- Suggested labels: ux, enhancement

8) Integration dev stack and test fixtures
- Priority: P1
- Effort: Medium
- Description: Provide a lightweight dev compose stack (test servers and test media via LFS or small fixtures) and smoke/integration tests similar to Jellyswarrm's dev/ integration approach.
- Acceptance: Contributors can run `just setup` / `make dev` to exercise basic flows locally.
- Suggested labels: infra, tests

9) SyncPlay / WebSocket session coordinator (design placeholder)
- Priority: P2
- Effort: Medium–High
- Description: Add a shared WebSocket transport so SyncPlay and remote control can coordinate sessions across multiple backends. See Jellyswarrm sessions for inspiration.
- Acceptance: Two clients can start a SyncPlay group and play content from different upstreams coordinated through JellyBloom.
- Suggested labels: enhancement, design
- See: docs/ADMIN_UI_SCAFFOLD.md (for UI integration points)

10) Licensing & CONTRIBUTING.md
- Priority: P0
- Effort: Low
- Description: Add an explicit open-source license (e.g., MIT or GPL) and a CONTRIBUTING.md with branch, PR, and issue templates.
- Acceptance: Clear legal status and contributor guidelines exist.
- Suggested labels: docs

---

Next steps
- Create GitHub Issues from these items (I can open them for you if you want). Two items have separate design placeholders created in docs/: the direct-playback design and the admin UI scaffold. Those files contain concrete design checkpoints and acceptance criteria to convert into implementation issues.

