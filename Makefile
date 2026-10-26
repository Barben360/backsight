PKG      := github.com/Barben360/backsight
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT   ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
LDFLAGS  := -s -w -X $(PKG)/internal/buildinfo.Version=$(VERSION) -X $(PKG)/internal/buildinfo.Commit=$(COMMIT)
# TEST_KEY (a minisign public key line) makes a test build, with the
# testkey tag, which trusts it too: never a release (verify-keys refuses
# the tag and the key).
LDFLAGS  += $(if $(TEST_KEY),-X $(PKG)/internal/signing.LinkedTestKey=$(TEST_KEY))
# TEST_HOOKS=1 adds the testhooks tag: CI's update proof (a migration that
# fails on demand). Never a release either (verify-keys refuses it).
comma    := ,
TAGLIST  := $(strip $(if $(TEST_KEY),testkey) $(if $(TEST_HOOKS),testhooks))
TAGS     := $(if $(TAGLIST),-tags $(subst $(eval) ,$(comma),$(TAGLIST)))
# TEST_IMAGE (with TEST_HOOKS) pins another Hindsight image: the update
# proof's server upgrade. Ignored without the tag.
LDFLAGS  += $(if $(TEST_IMAGE),-X $(PKG)/internal/server.TestImage=$(TEST_IMAGE))
DIST     ?= dist
OUT      ?= bin/backsight
PUBKEY   ?= minisign.pub
export CGO_ENABLED := 0

.PHONY: build test lint cross leaks snapshot verify-dist verify-keys
build:
	go build -trimpath $(TAGS) -ldflags "$(LDFLAGS)" -o $(OUT) ./cmd/backsight

# atexit_sleep_ms=0: no race runtime sleep (1 s) as each test binary and
# hook child exits; a GORACE of the caller's wins.
test:
	GORACE="$${GORACE-atexit_sleep_ms=0}" CGO_ENABLED=1 go test -race ./...


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

# Every artifact in dist/ (the archives, checksums.txt and the bare
# binaries in their build directories) must verify against the committed
# public key (PUBKEY: another one for a test release), and the release
# binary must trust the published keys only.
verify-dist:
	@set -e; n=0; for s in $(DIST)/*.minisig $(DIST)/*/*.minisig; do \
		[ -e "$$s" ] || continue; \
		minisign -VqH -p $(PUBKEY) -m "$${s%.minisig}"; n=$$((n+1)); \
	done; test $$n -gt 0; echo "$$n signatures verified"
	@$(MAKE) --no-print-directory verify-keys

# A release binary trusts minisign.pub and minisign.next.pub and nothing
# else, has no test hooks and pins the source's Hindsight image (decision
# 0067 and its block review): scripts/verify-keys.sh. Every target's build
# settings are checked (go version -m: no testkey or testhooks tag); the
# binary this machine runs is also asked (version --json). BIN is one
# binary and TARGET its os_arch (goreleaser's post-build hook passes every
# target, so a test build stops before anything is published); without
# BIN, every binary in DIST.
verify-keys:
	@scripts/verify-keys.sh "$(DIST)" "$(BIN)" "$(TARGET)"
