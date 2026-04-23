.PHONY: build install uninstall clean test

BINARY := mole
PREFIX ?= /usr/local
BINDIR := $(PREFIX)/bin

GOOS   ?= $(shell go env GOOS)
GOARCH ?= $(shell go env GOARCH)

build:
	GOOS=$(GOOS) GOARCH=$(GOARCH) go build -trimpath -ldflags="-s -w" -o $(BINARY) .
	@echo "built $(BINARY) for $(GOOS)/$(GOARCH)"

install: build
	@if [ ! -w "$(BINDIR)" ]; then \
		echo "$(BINDIR) is not writable — re-run with: sudo make install"; \
		exit 1; \
	fi
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
