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
# TEST_LOCK (with TEST_HOOKS) loads another native lock file in place of
# the embedded one: the native proof's upgrade to a second version.
LDFLAGS  += $(if $(TEST_LOCK),-X $(PKG)/internal/server/nativelock.TestLock=$(TEST_LOCK))
DIST     ?= dist
OUT      ?= bin/backsight
PUBKEY   ?= minisign.pub
export CGO_ENABLED := 0

.PHONY: build check test test-hooks bench-index lint cross leaks licenses snapshot verify-dist verify-keys
build:
	go build -trimpath $(TAGS) -ldflags "$(LDFLAGS)" -o $(OUT) ./cmd/backsight

# The gate before every commit, as CI runs it: formatting, vet, lint, the
# tests (the testhooks ones too), the release targets, and a secret scan of
# the working tree when gitleaks is installed (CI always runs it).
check:
	@files=$$(gofmt -l . | grep -v '^dist/' || true); \
		if [ -n "$$files" ]; then echo "gofmt: run gofmt -w on:"; echo "$$files"; exit 1; fi
	go vet ./...
	@$(MAKE) --no-print-directory lint
	@$(MAKE) --no-print-directory test
	@$(MAKE) --no-print-directory test-hooks
	@$(MAKE) --no-print-directory bench-index
	scripts/third-party-licenses.sh --check
	@$(MAKE) --no-print-directory cross >/dev/null
	@if command -v gitleaks >/dev/null 2>&1; then gitleaks dir . --no-banner --redact; \
		else echo "gitleaks is not installed: secret scan skipped (CI runs it)"; fi
	@echo "check: all passed"

# atexit_sleep_ms=0: no race runtime sleep (1 s) as each test binary and
# hook child exits; a GORACE of the caller's wins. internal/cli alone runs
# about 9 minutes under -race on the CI runner: go test's default 10-minute
# bound per package would cut it. The dashboard's page sweep (a year of
# history, every page) reads content, not concurrency: it skips under -race
# and runs in the second line, where -short leaves the time budgets to
# bench-index.
test:
	GORACE="$${GORACE-atexit_sleep_ms=0}" CGO_ENABLED=1 go test -race -timeout 20m ./...
	go test -short -count=1 ./internal/dashboard/

# The packages with test-only code behind the testhooks tag (the failing
# migration and server smoke of CI's update proof, the hook chaos switch),
# built and tested with it. A release never has the tag (verify-keys).
test-hooks:
	GORACE="$${GORACE-atexit_sleep_ms=0}" CGO_ENABLED=1 go test -race -tags testhooks ./internal/buildinfo/ ./internal/hook/ ./internal/migrate/ ./internal/server/ ./internal/server/nativelock/ ./internal/cli/

# The metrics index's time budgets on a year of generated history (first
# build, 15-minute catch-up), the page views' render budget and the
# dashboard's page budgets on the same history, then the index's two as
# benchmarks. Without -race, which skips the budget tests.
bench-index:
	go test -count=1 -run 'TestIndexBudgets' -v ./internal/index/
	go test -count=1 -run 'TestViewBudgets' -v ./internal/metrics/
	go test -count=1 -run 'TestPageBudgets|TestRenderBudgets' -v ./internal/dashboard/
	go test -run '^$$' -bench 'FirstBuild|CatchUp15' -benchtime 1x ./internal/index/

# --allow-parallel-runners: run beside another lint (a CI job, the gate)
# instead of failing on its lock.
lint:
	golangci-lint run --allow-parallel-runners

# Compile every release target (no tests: those run on Linux only until M7).
cross:
	@for t in linux/amd64 linux/arm64 darwin/arm64 windows/amd64; do \
		echo "== $$t"; GOOS=$${t%/*} GOARCH=$${t#*/} go build -o /dev/null ./... || exit 1; \
		GOOS=$${t%/*} GOARCH=$${t#*/} go vet ./... || exit 1; \
	done

leaks:
	gitleaks git . --no-banner --redact

# The third-party license texts embedded in the binary and shipped in every
# archive, regenerated from go.mod: run it after a dependency change and
# commit the file (CI and the release check it).
licenses:
	scripts/third-party-licenses.sh

# Local release build, signed with a throwaway key made for it and gone
# once it is verified (security review M3): only the release workflow ever
# signs with the release key, so no install or update accepts a snapshot.
# A release key in MINISIGN_KEY_FILE or MINISIGN_SECRET_KEY is refused, not
# set aside (scripts/release-key-guard.sh; RELEASE_PUBKEYS, for its tests:
# other public keys than minisign.pub and minisign.next.pub to refuse).
snapshot:
	@set -e; scripts/release-key-guard.sh --snapshot $(RELEASE_PUBKEYS); \
	d=$$(mktemp -d); trap 'rm -rf "$$d"' EXIT; \
	minisign -G -W -p "$$d/snapshot.pub" -s "$$d/snapshot.key" >/dev/null; \
	MINISIGN_KEY_FILE="$$d/snapshot.key" goreleaser release --snapshot --clean; \
	$(MAKE) --no-print-directory verify-dist PUBKEY="$$d/snapshot.pub"

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
