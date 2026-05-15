.PHONY: build test e2e-simple e2e-federation e2e-worktree

build:
	mkdir -p bin
	go build -o bin/zordon ./cmd/zordon/
	go build -o bin/alpha ./cmd/alpha/

test:
	go test ./...

e2e-simple: build
	cd examples/simple && sudo ../../bin/zordon sudo && ../../bin/zordon start --agent && ../../bin/zordon status --agent && ../../bin/zordon stop --agent

e2e-federation: build
	cd examples/federation && sudo ../../bin/zordon sudo && ../../bin/zordon start --agent && ../../bin/zordon status --agent && ../../bin/zordon stop --agent

e2e-worktree: build
	cd examples/worktree && sudo ../../bin/zordon sudo && ../../bin/zordon worktree create feature --agent && cd .zordon/worktrees/feature && ../../../../bin/zordon start --agent && ../../../../bin/zordon status --agent && ../../../../bin/zordon stop --agent

e2e: e2e-simple e2e-federation e2e-worktree
