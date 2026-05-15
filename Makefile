.PHONY: build test e2e-simple e2e-federation e2e-worktree

build:
	go build -o zordon ./cmd/zordon/
	go build -o alpha ./cmd/alpha/

test:
	go test ./...

e2e-simple: build
	cd examples/simple && sudo zordon sudo && ./zordon start --agent && ./zordon status --agent && ./zordon stop --agent

e2e-federation: build
	cd examples/federation && sudo zordon sudo && ./zordon start --agent && ./zordon status --agent && ./zordon stop --agent

e2e-worktree: build
	cd examples/worktree && sudo zordon sudo && ./zordon worktree create feature --agent && cd .zordon/worktrees/feature && ./zordon start --agent && ./zordon status --agent && ./zordon stop --agent

e2e: e2e-simple e2e-federation e2e-worktree
