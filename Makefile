.PHONY: build test e2e lint clean

EXAMPLES ?= $(shell ls -d examples/*/)

build:
	mkdir -p bin
	go build -o bin/zordon ./cmd/zordon/
	go build -o bin/alpha ./cmd/alpha/
	go build -o bin/tommy ./cmd/tommy/

# Go toolchain version the conformance fixtures pin via mise. Read
# from the golden service's go.mod (the actual `go` requirement the
# fixtures must satisfy) — single source of truth, no duplicate to
# drift. `make test` is the canonical "run the suite" entrypoint so it
# always pins; override with `ZORDON_TEST_GO_VERSION=1.27 make test`.
# Running bare `go test` without it keeps the deliberate no-toolchain
# (ambient Go) behavior.
ZORDON_TEST_GO_VERSION ?= $(shell sed -n 's/^go //p' golden/go/echo/go.mod)

# The harness drives prebuilt zordon/alpha/tommy (it never builds
# them); resolve via $ZORDON_BIN, $PATH, $ZORDON_TOMMY_BIN -> bin/.
#
# -p 1 -parallel 1: the conformance suite spawns real alpha/tommy
# against the SHARED <repo>/.zordon (one registry) on FIXED ports, so
# no two test binaries — and no two tests — may run concurrently or
# they collide. Correctness over speed (the Go build/mod cache absorbs
# most of the cost).
test: build
	ZORDON_BIN="$(CURDIR)/bin/zordon" \
	ZORDON_TOMMY_BIN="$(CURDIR)/bin/tommy" \
	ZORDON_TEST_GO_VERSION="$(ZORDON_TEST_GO_VERSION)" \
	PATH="$(CURDIR)/bin:$$PATH" \
	go test -cover -coverprofile=cover.out -count=2 -race -p 1 -parallel 1 ./...

lint:
	go tool staticcheck ./...

# Wipe every piece of local state that could let the NEXT `make test`
# pass without actually re-proving itself:
#
#   - go test cache: otherwise `go test` reprints a stale "ok (cached)"
#     and never re-runs — the #1 way a suite "passes" by accident.
#   - <repo>/.zordon + examples state: the shared ZORDON_HOME holds the
#     mise/toolchain cache, the host registry, per-run worktrees and
#     built SERVICE binaries. A cached toolchain or a stale service
#     binary can mask a broken integration; a leftover registry row can
#     satisfy a leftover-check that should have failed.
#   - leftover alpha/tommy daemons + $TMPDIR sockets: a still-running
#     supervisor or service holding a fixed test port makes an HTTP
#     assertion pass against a ghost from a previous run.
#   - build/coverage artifacts (bin/, *.out, alpha.log).
#
# Every step is best-effort and idempotent: a clean tree is a no-op,
# and nothing here can fail the target. $TMPDIR sockets in particular
# may be root-owned (a `sudo` federation provision ran there) or held
# by a stale process — un-removable, but harmless to the next run since
# each invocation keys its own state dir by fs-hash, so we never let
# them break `make clean`.
clean:
	-pkill -x tommy 2>/dev/null || true
	-pkill -x alpha 2>/dev/null || true
	-pkill -x zordon 2>/dev/null || true
	go clean -testcache
	rm -rf bin .zordon examples/.zordon examples/zordon cover.out
	rm -f *.out alpha.log examples/*/alpha.log
	-rm -rf "$${TMPDIR:-/tmp}"/zordon-* 2>/dev/null || true

# Each example owns a test.sh that asserts the claim it demonstrates
# (not just bringup). A missing prerequisite is a SKIP (exit 0), a
# broken claim is a hard failure.
e2e: build
	@for dir in $(EXAMPLES); do \
		t="$$dir/test.sh"; \
		[ -f "$$t" ] || { echo "no test.sh in $$dir, skipping"; continue; }; \
		echo "==> $$dir"; \
		bash "$$t" || exit 1; \
	done
