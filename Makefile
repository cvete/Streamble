VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X github.com/cvete/streamble/internal/api.Version=$(VERSION)

.PHONY: build run test test-short lint docker clean

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

docker:
	docker build --build-arg VERSION=$(VERSION) -t streamble:$(VERSION) .

clean:
	rm -rf bin dist
