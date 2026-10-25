PKG      := github.com/Barben360/backsight
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT   ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
LDFLAGS  := -s -w -X $(PKG)/internal/buildinfo.Version=$(VERSION) -X $(PKG)/internal/buildinfo.Commit=$(COMMIT)
# TEST_KEY (a minisign public key line) makes a test build, which trusts it
# too: never a release (verify-keys refuses it).
LDFLAGS  += $(if $(TEST_KEY),-X $(PKG)/internal/signing.TestKey=$(TEST_KEY))
# TEST_HOOKS=1 adds the testhooks tag: CI's update proof (a migration that
# fails on demand). Never a release either (verify-keys refuses it).
TAGS     := $(if $(TEST_HOOKS),-tags testhooks)
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
# else, and has no test hooks: a test key or the testhooks tag in a
# release fails it (decision 0067). BIN is the binary
# (default: this machine's in DIST); TARGET its os_arch, and another
# machine's binary is skipped: goreleaser's post-build hook passes every
# target, so a release with a test key stops before anything is published.
verify-keys:
	@set -e; host=$$(go env GOOS)_$$(go env GOARCH); \
	if [ -n "$(TARGET)" ] && [ "$(TARGET)" != "$$host" ]; then exit 0; fi; \
	b="$(BIN)"; [ -n "$$b" ] || b=$$(ls $(DIST)/backsight_$${host}*/backsight 2>/dev/null | head -n 1); \
	test -n "$$b" || { echo "verify-keys: no $$host binary in $(DIST)"; exit 1; }; \
	cur=$$(sed -n 's/^untrusted comment: minisign public key //p' minisign.pub); \
	nxt=$$(sed -n 's/^untrusted comment: minisign public key //p' minisign.next.pub); \
	want=$$(printf '[{"id":"%s","role":"current"},{"id":"%s","role":"next"}]' "$$cur" "$$nxt"); \
	"$$b" version --json | jq -e --argjson want "$$want" '.data.signing_keys == $$want and .data.test_key == false and .data.test_hooks == false' >/dev/null || { \
		echo "verify-keys: $$b trusts other keys than minisign.pub ($$cur) and minisign.next.pub ($$nxt), or has test hooks:"; \
		"$$b" version --json | jq -c '.data | {signing_keys, test_key, test_hooks}'; exit 1; }; \
	echo "$$b trusts $$cur and $$nxt only"
