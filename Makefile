.PHONY: build test e2e

EXAMPLES := $(shell ls -d examples/*/)

build:
	mkdir -p bin
	go build -o bin/zordon ./cmd/zordon/
	go build -o bin/alpha ./cmd/alpha/

test:
	go test ./...

e2e: build
	@for dir in $(EXAMPLES); do \
		echo "Running E2E for $$dir..."; \
		(cd $$dir && \
		if [ "$$(basename $$dir)" = "worktree" ]; then \
			../../bin/zordon worktree create feature --agent && cd .zordon/worktrees/feature; \
		fi && \
		../../bin/zordon start --agent && \
		../../bin/zordon status --agent && \
		../../bin/zordon stop --agent) || exit 1; \
	done
