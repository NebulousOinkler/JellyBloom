# JellyBloom container image. Builds with podman or docker:
#   podman build -t jellybloom -f Containerfile .
# Run with a config whose data_dir is /data (see config.example.yaml):
#   podman run -d --name jellybloom -p 8096:8096 \
#     -v ./config.yaml:/config/config.yaml:ro -v jellybloom-data:/data jellybloom
# Pair each upstream once (prints a Quick Connect code to approve):
#   podman run --rm -it -v ./config.yaml:/config/config.yaml:ro -v jellybloom-data:/data \
#     jellybloom pair -config /config/config.yaml home
# /data holds only the metadata database and the paired upstream tokens: never media.

FROM docker.io/library/golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
ARG TARGETOS TARGETARCH
# Pure Go (the SQLite driver too), so the binary is static.
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /out/jellybloom ./cmd/jellybloom

# Static base with CA certificates and a non-root user, no shell.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/jellybloom /usr/local/bin/jellybloom
VOLUME ["/data"]
EXPOSE 8096
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/jellybloom"]
CMD ["-config", "/config/config.yaml"]
