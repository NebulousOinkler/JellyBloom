# JellyBloom

Aggregates multiple Jellyfin servers (public or local) behind a single Jellyfin-compatible server,
combining their libraries with set operations (union, intersection, difference).

## Status
Early scaffold. Config loading and a stub `/System/Info/Public` endpoint only.

## Run
    cp config.example.yaml config.yaml        # add your upstreams and a user
    go run ./cmd/jellybloom hash-password      # for the user's password_hash
    go run ./cmd/jellybloom pair -config config.yaml home   # once per upstream
    go run ./cmd/jellybloom -config config.yaml

## Design
See [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) for the architecture and build plan.
