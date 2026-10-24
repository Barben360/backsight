PKG      := github.com/Barben360/backsight
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT   ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
LDFLAGS  := -s -w -X $(PKG)/internal/buildinfo.Version=$(VERSION) -X $(PKG)/internal/buildinfo.Commit=$(COMMIT)
export CGO_ENABLED := 0

.PHONY: build test lint cross leaks snapshot verify-dist
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

# Signed local release build; the key comes from pass and never touches the
# repository (decision 0027).
snapshot:
	@set -e; d=$$(mktemp -d); trap 'shred -u "$$d/key" 2>/dev/null; rm -rf "$$d"' EXIT; \
	pass show backsight/minisign-secret-key > "$$d/key"; \
	MINISIGN_KEY_FILE="$$d/key" goreleaser release --snapshot --clean
	@$(MAKE) --no-print-directory verify-dist

# Every artifact in dist/ must verify against the committed public key.
verify-dist:
	@set -e; n=0; for s in dist/*.minisig; do \
		minisign -Vq -p minisign.pub -m "$${s%.minisig}"; n=$$((n+1)); \
	done; test $$n -gt 0; echo "$$n signatures verified"
