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
ENV BB_DATA=/data
VOLUME /data
EXPOSE 8443
HEALTHCHECK --interval=30s --timeout=10s --start-period=20s CMD ["/usr/local/bin/bb", "status"]
ENTRYPOINT ["/usr/local/bin/bb-server"]
CMD ["serve"]
