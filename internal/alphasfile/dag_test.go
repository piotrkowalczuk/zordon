package alphasfile

import (
	"testing"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
)

func TestProducerNodeFromTrav_moduleRoot(t *testing.T) {
	cases := map[string]struct {
		expr       string
		selfModule string
		wantSvc    string
		wantID     string
		ok         bool
	}{
		"module ref producer":         {"module.a.service.go.x.vars.p", "", "module.a.service.go.x", "module.a.service.go.x.vars", true},
		"module ref file":             {"module.a.service.go.x.file.cfg.path", "", "module.a.service.go.x", "module.a.service.go.x.file.cfg", true},
		"module ref static":           {"module.a.service.go.x.runtime.ready", "", "", "", false},
		"module toolchain":            {"module.a.toolchain.go.ready", "", "", "", false},
		"bare service in module":      {"service.go.x.vars.p", "m", "module.m.service.go.x", "module.m.service.go.x.vars", true},
		"bare service default module": {"service.go.x.env.K", "", "service.go.x", "service.go.x.env", true},
		"self in module":              {"self.vars.p", "m", "module.m.service.go.me", "module.m.service.go.me.vars", true},
	}
	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			trav, diags := hclsyntax.ParseTraversalAbs([]byte(c.expr), "t.hcl", hcl.InitialPos)
			if diags.HasErrors() {
				t.Fatal(diags.Error())
			}
			svc, id, ok := producerNodeFromTrav(trav, ServiceRef(c.selfModule, "go", "me"), c.selfModule)
			if ok != c.ok || svc != c.wantSvc || id != c.wantID {
				t.Errorf("got (%q,%q,%v), want (%q,%q,%v)", svc, id, ok, c.wantSvc, c.wantID, c.ok)
			}
		})
	}
}
