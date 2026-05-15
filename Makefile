.PHONY: build test e2e

EXAMPLES ?= $(shell ls -d examples/*/)

build:
	mkdir -p bin
	go build -o bin/zordon ./cmd/zordon/
	go build -o bin/alpha ./cmd/alpha/

test:
	go test ./...

e2e: build
	@ZORDON_BIN="$$(pwd)/bin/zordon"; \
	for dir in $(EXAMPLES); do \
		echo "Running E2E for $$dir..."; \
		(cd $$dir && \
		if [ "$$(basename $$dir)" = "federation" ]; then sudo -v; fi && \
		$$ZORDON_BIN sudo && \
		if [ "$$(basename $$dir)" = "worktree" ]; then \
			rm -rf .zordon/worktrees/feature && \
			$$ZORDON_BIN worktree create feature && \
			cd .zordon/worktrees/feature; \
		fi && \
		$$ZORDON_BIN start --agent && \
		$$ZORDON_BIN status --agent && \
		$$ZORDON_BIN stop --agent) || exit 1; \
	done
