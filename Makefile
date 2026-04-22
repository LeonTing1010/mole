.PHONY: build install uninstall clean test

BINARY := mole
PREFIX ?= /usr/local
BINDIR := $(PREFIX)/bin

ARCH := $(shell uname -m)
GOARCH := $(shell if [ "$(ARCH)" = "x86_64" ]; then echo "amd64"; else echo "arm64"; fi)

build:
	GOARCH=$(GOARCH) go build -trimpath -ldflags="-s -w" -o $(BINARY) .
	@echo "built $(BINARY) for $$(GOARCH)"

install: build
	install -m 755 $(BINARY) $(BINDIR)/$(BINARY)
	@echo "installed to $(BINDIR)/$(BINARY)"
	@command -v sing-box >/dev/null || echo "note: sing-box not found in PATH — run: brew install sing-box"

uninstall:
	rm -f $(BINDIR)/$(BINARY)

clean:
	rm -f $(BINARY)
	rm -rf $(BINARY).dSYM

test:
	go test ./...