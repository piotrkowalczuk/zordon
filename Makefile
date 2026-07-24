.PHONY: fmt build build.race build.release test test.fast test.race test.coverage test.unit test.coverage.unit test.conformance.go test.conformance.rust test.conformance.node test.conformance.pkg test.conformance.java e2e lint gen release.check release.snapshot clean

EXAMPLES ?= $(shell ls -d examples/*/)
GOTEST_TIMEOUT ?= 30m

# Conformance suites are gated behind per-toolchain build tags (see
# tests/conformance/*_test.go): a plain `go test ./...` stays fast and each
# toolchain runs on its own CI leg. CONFORMANCE_TAGS is the full set — every
# conformance test — used by the whole-module test.race/test.coverage passes.
CONFORMANCE_TAGS ?= conformance_go conformance_rust conformance_node conformance_pkg conformance_java
ZORDON_TEST_ENV = ZORDON_BIN="$(CURDIR)/bin/zordon" ZORDON_TOMMY_BIN="$(CURDIR)/bin/tommy" PATH="$(CURDIR)/bin:$$PATH"

# Comma-separated form of the tag set: GOFLAGS=-tags can't hold spaces (they'd
# split into separate flags). Used to thread the tags through `make lint`.
empty :=
space := $(empty) $(empty)
comma := ,
CONFORMANCE_TAGS_CSV := $(subst $(space),$(comma),$(strip $(CONFORMANCE_TAGS)))

fmt:
	go fix ./...
	gofmt -s -w $$(find . -type d -name .zordon -prune -o -name '*.go' -print)

build:
	mkdir -p bin
	go build -o bin/zordon ./cmd/zordon/
	go build -o bin/alpha ./cmd/alpha/
	go build -o bin/tommy ./cmd/tommy/

# Race-instrumented service binaries, so the supervisor's own goroutines are
# checked when tests spawn them (conformance/e2e), not just the test process.
build.race:
	mkdir -p bin
	go build -race -o bin/zordon ./cmd/zordon/
	go build -race -o bin/alpha ./cmd/alpha/
	go build -race -o bin/tommy ./cmd/tommy/

# Distribution binaries: -trimpath for reproducibility, -s -w to strip the
# debug info and symbol table (smaller binaries).
build.release:
	mkdir -p bin
	go build -trimpath -ldflags "-s -w" -o bin/zordon ./cmd/zordon/
	go build -trimpath -ldflags "-s -w" -o bin/alpha ./cmd/alpha/
	go build -trimpath -ldflags "-s -w" -o bin/tommy ./cmd/tommy/

# `test` is two decoupled passes so a coverage-report-writer hiccup can't sink
# the correctness signal, and each runs against the build it needs.
test: test.race test.coverage

# Fast local loop: no build tags, so the conformance package contributes only
# its static (no-bringup) tests. Finishes in seconds — use it while iterating.
test.fast:
	go test ./...

test.race: build.race
	$(ZORDON_TEST_ENV) \
	go test -timeout $(GOTEST_TIMEOUT) -count=1 -race -tags '$(CONFORMANCE_TAGS)' ./...

test.coverage: build
	$(ZORDON_TEST_ENV) \
	go test -timeout $(GOTEST_TIMEOUT) -cover -coverpkg=./... -coverprofile=cover.out -tags '$(CONFORMANCE_TAGS)' ./...

# test.unit / test.coverage.unit run the whole module WITHOUT the conformance
# build tags, so the tagged conformance bringups are skipped — they run in the
# per-toolchain legs below. The conformance package still contributes its
# static (no-bringup) tests, and a few internal packages exercise real
# toolchains, so the race binaries and a zordon home are still required.
test.unit: build.race
	$(ZORDON_TEST_ENV) \
	go test -timeout $(GOTEST_TIMEOUT) -count=1 -race ./...

test.coverage.unit: build
	$(ZORDON_TEST_ENV) \
	go test -timeout $(GOTEST_TIMEOUT) -cover -coverpkg=./... -coverprofile=cover.out ./...

# Per-toolchain conformance legs — CI runs one per matrix cell so each installs
# only its own toolchain. Race-instrumented binaries are built first so the
# supervisor's goroutines are checked when a test spawns them.
test.conformance.go: build.race
	$(ZORDON_TEST_ENV) \
	go test -timeout $(GOTEST_TIMEOUT) -race -tags conformance_go ./tests/conformance/

test.conformance.rust: build.race
	$(ZORDON_TEST_ENV) \
	go test -timeout $(GOTEST_TIMEOUT) -race -tags conformance_rust ./tests/conformance/

test.conformance.node: build.race
	$(ZORDON_TEST_ENV) \
	go test -timeout $(GOTEST_TIMEOUT) -race -tags conformance_node ./tests/conformance/

test.conformance.pkg: build.race
	$(ZORDON_TEST_ENV) \
	go test -timeout $(GOTEST_TIMEOUT) -race -tags conformance_pkg ./tests/conformance/

test.conformance.java: build.race
	$(ZORDON_TEST_ENV) \
	go test -timeout $(GOTEST_TIMEOUT) -race -tags conformance_java ./tests/conformance/

# The conformance suites compile only under their per-toolchain build tags, so
# the linters must see the full set — otherwise they flag the shared helpers as
# unused (U1000) and skip the tagged files entirely. GOFLAGS threads -tags
# through every go-toolchain-based linter uniformly (go-critic rejects a bare
# -tags flag but honors GOFLAGS). Scoped to `lint` so it never leaks into
# `go build`/`go test` (where it would trigger conformance bringups).
lint: export GOFLAGS = -tags=$(CONFORMANCE_TAGS_CSV)
lint:
	go vet ./...
	go tool staticcheck ./...
	go tool go-critic check ./...
	go tool gosec -exclude-dir=.claude -exclude-dir=.zordon -exclude-dir=examples -exclude=G204,G304 ./...
	go tool govulncheck ./...

gen:
	go generate ./...
	mkdocs build --strict --site-dir _site
	python3 scripts/agent-skills-index.py _site

release.check:
	goreleaser check

# Full release build into dist/ without tagging or publishing anything.
release.snapshot:
	goreleaser release --snapshot --clean
	python3 scripts/build-mcpb.py --dist dist


clean:
	-pkill -x tommy 2>/dev/null || true
	-pkill -x alpha 2>/dev/null || true
	-pkill -x zordon 2>/dev/null || true
	go clean -testcache
	rm -rf bin dist _site .zordon examples/.zordon examples/zordon *.out
	rm -f *.out alpha.log examples/*/alpha.log
	-rm -rf "$${TMPDIR:-/tmp}"/zordon-* 2>/dev/null || true

e2e: build
	@for dir in $(EXAMPLES); do \
		base=$$(basename $$dir); \
		case $$base in \
			*_macos) [ "$$(uname -s)" = Darwin ] || { echo "==> $$dir (SKIP: macOS-only)"; continue; } ;; \
			*_linux) [ "$$(uname -s)" = Linux ]  || { echo "==> $$dir (SKIP: Linux-only)"; continue; } ;; \
		esac; \
		t="$$dir/test.sh"; \
		[ -f "$$t" ] || { echo "no test.sh in $$dir, skipping"; continue; }; \
		echo "==> $$dir"; \
		bash "$$t" || exit 1; \
	done
