.PHONY: build test e2e

EXAMPLES ?= $(shell ls -d examples/*/)

build:
	mkdir -p bin
	go build -o bin/zordon ./cmd/zordon/
	go build -o bin/alpha ./cmd/alpha/

test:
	go test -cover -coverprofile=cover.out -count=2 -race ./...

lint:
	go tool staticcheck ./...

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
