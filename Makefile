VERSION ?= $(shell git describe --always --dirty --tags 2>/dev/null || echo dev)
PREFIX ?= /usr/local
LDFLAGS := -X main.version=$(VERSION)

.PHONY: build install test

# The normal build path stamps the executable with the source revision. `dev`
# remains an intentional fallback for a source tree without git metadata.
build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o factoryd ./cmd/factoryd

install: build
	install -D -m 0755 factoryd $(DESTDIR)$(PREFIX)/bin/factoryd

test:
	go test -race -count=1 ./...
