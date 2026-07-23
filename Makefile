.PHONY: build test vet dist clean install

build:
	go build -o bin/nexus ./cmd/nexus

test:
	go test ./...

vet:
	go vet ./...

dist:
	./scripts/build.sh

install: build
	install -m 0755 bin/nexus $(HOME)/.local/bin/nexus

clean:
	rm -rf bin dist
