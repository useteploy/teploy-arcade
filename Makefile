BINARY := teploy-arcade
VERSION ?= dev
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.date=$(DATE)

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/$(BINARY) ./cmd/$(BINARY)

test:
	go test ./... -count=1
	$(MAKE) test-frontend

test-frontend:
	@if command -v node >/dev/null 2>&1; then \
		for test_file in test/*.test.js; do \
			node "$$test_file" || exit $$?; \
		done; \
	else \
		echo "node not found, skipping frontend tests"; \
	fi

# The panel mutates server state from runner goroutines while HTTP handlers
# read it. The detector found real races on Server.Status; keep it in CI.
race:
	go test -race ./... -count=1

vet:
	go vet ./...

clean:
	rm -rf bin dist

.PHONY: build test test-frontend race vet clean
