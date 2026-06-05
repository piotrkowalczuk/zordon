package main

import (
	"testing"

	"github.com/piotrkowalczuk/zordon/internal/protocol"
)

// canReuse encodes the multi-agent invariant: a `zordon start` in a leaf dir
// must never restart a healthy, unchanged parent (shared infra other agents
// depend on), but always (re)starts the leaf itself.
func TestCanReuse(t *testing.T) {
	const hash = "cfg-abc"
	running := &protocol.StateInfo{CfgHash: hash}
	drifted := &protocol.StateInfo{CfgHash: "cfg-xyz"}

	cases := map[string]struct {
		isLeaf bool
		old    *protocol.StateInfo
		cfg    string
		want   bool
	}{
		"parent running, hash matches": {isLeaf: false, old: running, cfg: hash, want: true},
		"parent running, drifted":      {isLeaf: false, old: drifted, cfg: hash, want: false},
		"parent not running":           {isLeaf: false, old: nil, cfg: hash, want: false},
		"leaf running, hash matches":   {isLeaf: true, old: running, cfg: hash, want: false},
		"leaf running, drifted":        {isLeaf: true, old: drifted, cfg: hash, want: false},
		"leaf not running":             {isLeaf: true, old: nil, cfg: hash, want: false},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if got := canReuse(c.isLeaf, c.old, c.cfg); got != c.want {
				t.Errorf("canReuse(isLeaf=%v, old=%v, cfg=%q) = %v, want %v",
					c.isLeaf, c.old, c.cfg, got, c.want)
			}
		})
	}
}
