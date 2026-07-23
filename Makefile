.PHONY: build test vet fmt check dist clean install

# Stamped into the binary so `nexus --version` reports something useful
# rather than "dev (none)". Overridable: make build VERSION=0.2.0
VERSION ?= $(shell git describe --tags --always 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
LDFLAGS := -s -w -X main.Version=$(VERSION) -X main.Commit=$(COMMIT)

build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/nexus ./cmd/nexus

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -w ./cmd ./internal

# The pre-release gate. CI config lives outside this repo, so this is the
# thing to run before tagging.
check:
	@test -z "$$(gofmt -l ./cmd ./internal)" || { echo "needs gofmt:"; gofmt -l ./cmd ./internal; exit 1; }
	go vet ./...
	go test ./...

dist:
	./scripts/build.sh

install: build
	install -m 0755 bin/nexus $(HOME)/.local/bin/nexus

clean:
	rm -rf bin dist
