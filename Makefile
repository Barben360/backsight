PKG      := github.com/Barben360/backsight
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT   ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
LDFLAGS  := -s -w -X $(PKG)/internal/buildinfo.Version=$(VERSION) -X $(PKG)/internal/buildinfo.Commit=$(COMMIT)
export CGO_ENABLED := 0

.PHONY: build test lint cross leaks
build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/backsight ./cmd/backsight

test:
	CGO_ENABLED=1 go test -race ./...

lint:
	golangci-lint run

# Compile every release target (no tests: those run on Linux only until M7).
cross:
	@for t in linux/amd64 linux/arm64 darwin/arm64 windows/amd64; do \
		echo "== $$t"; GOOS=$${t%/*} GOARCH=$${t#*/} go build -o /dev/null ./... || exit 1; \
		GOOS=$${t%/*} GOARCH=$${t#*/} go vet ./... || exit 1; \
	done

leaks:
	gitleaks git . --no-banner --redact
