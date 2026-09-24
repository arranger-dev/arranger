# make          build ./arranger
# make run      build and start it
# make dev      run from source with debug logs and a throwaway data dir, on port 7778
# make check    formatting, vet and tests
# make install  copy ./arranger to /usr/local/bin
# make dist     release archives for macOS and Linux in dist/
# make clean    remove build output

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse HEAD 2>/dev/null)
LDFLAGS := -s -w -X arranger/internal/version.Version=$(VERSION) \
	-X arranger/internal/version.Commit=$(COMMIT) \
	-X arranger/internal/version.Built=$(shell date -u +%Y-%m-%dT%H:%M:%SZ)
GOBUILD := CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)"

.PHONY: build run dev check install dist clean

build:
	$(GOBUILD) -o arranger ./cmd/arranger

run: build
	./arranger

dev:
	go run ./cmd/arranger -log debug -data .dev-data -addr 127.0.0.1:7778

check:
	test -z "$$(gofmt -l .)"
	go vet ./...
	go test ./...

install: build
	install -m 755 arranger /usr/local/bin/arranger

dist:
	rm -rf dist && mkdir dist
	for p in darwin/amd64 darwin/arm64 linux/amd64 linux/arm64; do \
		GOOS=$${p%/*} GOARCH=$${p#*/} $(GOBUILD) -o dist/arranger ./cmd/arranger && \
		tar czf dist/arranger_$${p%/*}_$${p#*/}.tar.gz -C dist arranger || exit 1; \
	done
	rm dist/arranger
	cd dist && shasum -a 256 *.tar.gz > checksums.txt

clean:
	rm -rf arranger dist
