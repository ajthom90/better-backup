VERSION ?= dev
LDFLAGS := -s -w -X github.com/ajthom90/better-backup/internal/server.Version=$(VERSION) -X github.com/ajthom90/better-backup/internal/agent.Version=$(VERSION)

.PHONY: build app test docker recovery
build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/ ./cmd/bb ./cmd/bb-agent ./cmd/bb-server

# The desktop app for this machine. It shows a web view, so it needs cgo
# and, on Linux, the GTK 3 and WebKitGTK 4.1 development packages.
APP_TAGS := desktop,production
ifeq ($(shell go env GOOS),linux)
APP_TAGS := $(APP_TAGS),webkit2_41
endif
app:
	CGO_ENABLED=1 go build -trimpath -tags $(APP_TAGS) -ldflags "$(LDFLAGS)" -o bin/ ./cmd/bb-agent-ui

test:
	go vet ./...
	go test -race -count=1 ./...

docker:
	docker build --build-arg VERSION=$(VERSION) --target server -t better-backup:$(VERSION) .
	docker build --build-arg VERSION=$(VERSION) --target agent -t better-backup-agent:$(VERSION) .

# The bare-metal recovery disc. Needs the tools listed in
# packaging/recovery/build.py; the result goes to bin/recovery.iso.
recovery:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/recovery/bb-agent ./cmd/bb-agent
	python3 packaging/recovery/build.py --agent bin/recovery/bb-agent --out bin/recovery.iso --firmware --cache bin/recovery-cache --version $(VERSION)
