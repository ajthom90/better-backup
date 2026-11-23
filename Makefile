VERSION ?= dev
LDFLAGS := -s -w -X github.com/ajthom90/better-backup/internal/server.Version=$(VERSION) -X github.com/ajthom90/better-backup/internal/agent.Version=$(VERSION)

.PHONY: build test docker
build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/ ./cmd/...

test:
	go vet ./...
	go test -race -count=1 ./...

docker:
	docker build --build-arg VERSION=$(VERSION) --target server -t better-backup:$(VERSION) .
	docker build --build-arg VERSION=$(VERSION) --target agent -t better-backup-agent:$(VERSION) .
