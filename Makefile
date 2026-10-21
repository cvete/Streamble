VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X github.com/cvete/streamble/internal/api.Version=$(VERSION)

.PHONY: build run test test-short lint ui ui-check docker clean

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/streamble ./cmd/streamble

run: build
	./bin/streamble $(if $(wildcard streamble.yml),-config streamble.yml,)

# Full suite, including end-to-end tests with FFmpeg (skipped if not installed).
test:
	go test -race -count=1 ./...

# Unit tests only.
test-short:
	go test -race -count=1 $(shell go list ./... | grep -v /e2e)

lint:
	gofmt -l . | (! grep .) || (echo "run gofmt -w ." && exit 1)
	go vet ./...

# Control panel: builds web/panel into internal/panel/dist, which is
# committed and embedded in the binary, so Go builds need no Node.js.
ui:
	cd web/panel && npm ci && npm run build

# Fails when the committed panel build is out of date with web/panel.
ui-check: ui
	test -z "$$(git status --porcelain -- internal/panel/dist)" || (git status --short -- internal/panel/dist; echo "run make ui and commit internal/panel/dist"; exit 1)

docker:
	docker build --build-arg VERSION=$(VERSION) -t streamble:$(VERSION) .

clean:
	rm -rf bin dist
