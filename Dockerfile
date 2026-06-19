# Static Go binary that shells out to a pinned alpine ffmpeg. The orchestration
# is pure Go (CGO_ENABLED=0 → portable across any Linux host); ffmpeg does the
# transcoding. Multi-arch capable (linux/amd64, linux/arm64) via buildx.
FROM --platform=$BUILDPLATFORM golang:1.24 AS build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY core ./core
COPY cmd ./cmd
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/transcoder ./cmd/transcoder

FROM alpine:3.20
RUN apk add --no-cache ffmpeg ca-certificates tini
COPY --from=build /out/transcoder /usr/local/bin/transcoder
# tini reaps zombies and forwards SIGTERM so docker stop / k8s drains cleanly.
ENTRYPOINT ["/sbin/tini", "--", "/usr/local/bin/transcoder"]
