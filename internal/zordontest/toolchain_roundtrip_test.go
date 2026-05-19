package zordontest

import (
	"strings"
	"testing"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/gohcl"
	"github.com/hashicorp/hcl/v2/hclparse"

	"github.com/piotrkowalczuk/zordon/internal/alphasfile"
)

// mirrors zordon's decode shape just enough to prove the encoded block
// is schema-valid (decodes without diags) — the real round-trip.
type rtGo struct {
	Version string         `hcl:"version"`
	Tools   hcl.Expression `hcl:"tools,optional"`
	Env     hcl.Expression `hcl:"env,optional"`
}
type rtTC struct {
	Go *rtGo `hcl:"go,block"`
}
type rtRoot struct {
	Toolchain *rtTC `hcl:"toolchain,block"`
}

func TestGoToolchainHCL_SerializeDeserializeRoundtrip(t *testing.T) {
	saved := *goVersionFlag
	defer func() { *goVersionFlag = saved }()

	// Gate: absent flag ⇒ inject nothing.
	*goVersionFlag = ""
	if s := goToolchainHCL(alphasfile.ToolchainConfig{}); s != "" {
		t.Fatalf("unset flag must inject nothing, got:\n%s", s)
	}

	// Present ⇒ serialize → deserialize round-trip.
	*goVersionFlag = "1.26.2"
	got := goToolchainHCL(alphasfile.ToolchainConfig{
		Tools: map[string]string{"dlv": "1.22.0"},
		Env:   map[string]string{"CGO_ENABLED": "0"},
	})
	t.Logf("encoded:\n%s", got)
	if !strings.Contains(got, *goVersionFlag) {
		t.Fatalf("encoded block missing flag version %q:\n%s", *goVersionFlag, got)
	}
	f, diags := hclparse.NewParser().ParseHCL([]byte(got), "toolchain.hcl")
	if diags.HasErrors() {
		t.Fatalf("encoded HCL does not parse: %v", diags)
	}
	var root rtRoot
	if d := gohcl.DecodeBody(f.Body, nil, &root); d.HasErrors() {
		t.Fatalf("encoded block does not decode against the grammar: %v", d)
	}
	if root.Toolchain == nil || root.Toolchain.Go == nil || root.Toolchain.Go.Version != *goVersionFlag {
		t.Fatalf("round-trip lost the version: %+v", root)
	}
}
