# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.26 AS build
ARG TARGETOS TARGETARCH VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
    -ldflags "-s -w -X github.com/ajthom90/better-backup/internal/server.Version=$VERSION -X github.com/ajthom90/better-backup/internal/agent.Version=$VERSION" \
    -o /out/ ./cmd/...
# The recovery discs start x64 and ARM64 machines, whatever the server
# runs on.
RUN for arch in amd64 arm64; do CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -trimpath \
    -ldflags "-s -w -X github.com/ajthom90/better-backup/internal/server.Version=$VERSION -X github.com/ajthom90/better-backup/internal/agent.Version=$VERSION" \
    -o /out/recovery/$arch/bb-agent ./cmd/bb-agent || exit 1; done

# The bare-metal recovery discs, which the server offers for download.
# Both are put together by x64 tools.
FROM --platform=linux/amd64 ubuntu:24.04 AS recovery
ARG VERSION=dev
RUN apt-get update && apt-get install -y --no-install-recommends python3 ca-certificates file cpio xorriso mtools \
    grub-pc-bin grub-common grub-efi-amd64-signed shim-signed kmod zstd busybox-static && rm -rf /var/lib/apt/lists/*
COPY packaging/recovery/build.py /build.py
COPY --from=build /out/recovery/ /agents/
RUN apt-get update && python3 /build.py --agent /agents/amd64/bb-agent --out /recovery.iso --firmware --version "$VERSION" \
    && python3 /build.py --arch arm64 --agent /agents/arm64/bb-agent --out /recovery-arm64.iso --firmware --version "$VERSION" \
    && rm -rf /var/lib/apt/lists/*

# The agent image, for backing up a container host: mount what to protect
# and run as root. Build with: docker build --target agent .
FROM gcr.io/distroless/static-debian12 AS agent
COPY --from=build /out/bb-agent /usr/local/bin/
ENV BB_AGENT_DIR=/var/lib/better-backup-agent
VOLUME /var/lib/better-backup-agent
ENTRYPOINT ["/usr/local/bin/bb-agent"]
CMD ["run"]

# The server image (the default target).
FROM gcr.io/distroless/static-debian12 AS server
COPY --from=build /out/bb-server /out/bb /usr/local/bin/
COPY --from=recovery /recovery.iso /recovery-arm64.iso /usr/share/better-backup/
# Agent installers for the web UI to hand out; see packaging/installers.
COPY packaging/installers/ /usr/local/bin/installers/
ENV BB_DATA=/data
VOLUME /data
EXPOSE 8443
HEALTHCHECK --interval=30s --timeout=10s --start-period=20s CMD ["/usr/local/bin/bb", "status"]
ENTRYPOINT ["/usr/local/bin/bb-server"]
CMD ["serve"]
