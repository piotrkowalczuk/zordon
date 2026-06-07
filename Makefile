.PHONY: fmt build test e2e lint gen clean

EXAMPLES ?= $(shell ls -d examples/*/)

fmt:
	go fix ./...
	gofmt -s -w $$(find . -type d -name .zordon -prune -o -name '*.go' -print)

build:
	mkdir -p bin
	go build -o bin/zordon ./cmd/zordon/
	go build -o bin/alpha ./cmd/alpha/
	go build -o bin/tommy ./cmd/tommy/

test: build
	ZORDON_BIN="$(CURDIR)/bin/zordon" \
	ZORDON_TOMMY_BIN="$(CURDIR)/bin/tommy" \
	PATH="$(CURDIR)/bin:$$PATH" \
	go test -cover -coverpkg=./... -coverprofile=cover.out -count=2 -race  ./...

lint:
	go vet ./...
	go tool staticcheck ./...
	go tool go-critic check ./...
	go tool gosec -exclude-dir=.claude -exclude-dir=.zordon -exclude-dir=examples -exclude=G204,G304 ./...
	go tool govulncheck ./...

gen:
	go generate ./...
	mkdocs build --strict --site-dir _site


clean:
	-pkill -x tommy 2>/dev/null || true
	-pkill -x alpha 2>/dev/null || true
	-pkill -x zordon 2>/dev/null || true
	go clean -testcache
	rm -rf bin _site .zordon examples/.zordon examples/zordon *.out
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
