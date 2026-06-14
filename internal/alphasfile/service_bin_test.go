package alphasfile

import (
	"strings"
	"testing"
)

func TestEnvOpSentinel_roundTrip(t *testing.T) {
	// ParseEnvOp runs after SubstituteServiceBins, so its dirs are NUL-free.
	cases := map[string]struct {
		op   string
		args []string
	}{
		"prepend one dir":    {"prepend", []string{"/pg/bin"}},
		"append two dirs":    {"append", []string{"/a", "/b"}},
		"path-list fragment": {"prepend", []string{"/d1:/d2"}},
	}
	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			op, args, ok := ParseEnvOp(EnvOpSentinel(c.op, c.args))
			if !ok {
				t.Fatal("ParseEnvOp returned ok=false for a directive it produced")
			}
			if op != c.op {
				t.Errorf("op = %q, want %q", op, c.op)
			}
			if len(args) != len(c.args) {
				t.Fatalf("args = %q, want %q", args, c.args)
			}
			for i := range args {
				if args[i] != c.args[i] {
					t.Errorf("args[%d] = %q, want %q", i, args[i], c.args[i])
				}
			}
		})
	}
}

func TestSubstituteBins_thenParse(t *testing.T) {
	// The realistic chain: env::prepend(fs::service::bin(X), "/lit") at eval
	// embeds a bin sentinel inside the directive; the substitution pass swaps
	// it for the resolved dir, and only then does ParseEnvOp split. Covers
	// both a "svc" and a "tc" ref in one directive.
	resolve := func(kind, ref string) []string {
		switch {
		case kind == "svc" && ref == "service.pkg.postgres":
			return []string{"/install/postgres/bin"}
		case kind == "tc" && ref == "go":
			return []string{"/install/go/bin"}
		}
		return nil
	}
	dir := EnvOpSentinel("prepend", []string{BinSentinel("svc", "service.pkg.postgres"), BinSentinel("tc", "go"), "/lit"})
	op, args, ok := ParseEnvOp(SubstituteBins(dir, resolve))
	if !ok || op != "prepend" {
		t.Fatalf("parse: ok=%v op=%q", ok, op)
	}
	if len(args) != 3 || args[0] != "/install/postgres/bin" || args[1] != "/install/go/bin" || args[2] != "/lit" {
		t.Fatalf("args = %q, want [/install/postgres/bin /install/go/bin /lit]", args)
	}
}

func TestSubstituteBins_unresolvedDropsToken(t *testing.T) {
	got := SubstituteBins("a"+BinSentinel("svc", "service.pkg.ghost")+"b", func(string, string) []string { return nil })
	if got != "ab" {
		t.Errorf("unresolved sentinel should vanish: got %q", got)
	}
}

func TestParseEnvOp_rejectsNonDirective(t *testing.T) {
	// Plain values, an unrelated sentinel, and a bare bin sentinel must all
	// read as "not a directive" so the runtime overlays them verbatim.
	for _, v := range []string{"", "/usr/bin:/bin", ArgSentinel("port"), BinSentinel("svc", "service.pkg.postgres")} {
		if _, _, ok := ParseEnvOp(v); ok {
			t.Errorf("ParseEnvOp(%q) = ok; want false for a non-directive value", v)
		}
	}
}

func TestServiceBin_compilesToDirective(t *testing.T) {
	af := compile(t, `
service "pkg" "postgres" {
  package = "postgres@16.4"
  vars = { port = net::pickport() }
  runtime { cmd = ["postgres"] }
}

service "pkg" "redis" {
  package = "redis@7.2.5"
  runtime {
    provision "dump" {
      env = { PATH = env::prepend(fs::service::bin(service.pkg.postgres)) }
      cmd = "pg_dump > /tmp/x.sql"
    }
    cmd = ["redis-server"]
  }
}
`, nil)
	svc := svcByName(af, "redis")
	if svc == nil || svc.Runtime == nil || len(svc.Runtime.Provision) != 1 {
		t.Fatalf("redis provision not resolved: %+v", svc)
	}
	pathVal := svc.Runtime.Provision[0].Env["PATH"]
	// The eval output embeds the postgres svcbin sentinel inside a prepend
	// directive; substitution (with a stand-in resolver) then ParseEnvOp is
	// the same chain alpha runs.
	resolved := SubstituteBins(pathVal, func(kind, ref string) []string {
		if kind == "svc" && ref == serviceID("pkg", "postgres") {
			return []string{"/install/pg/bin"}
		}
		return nil
	})
	op, args, ok := ParseEnvOp(resolved)
	if !ok {
		t.Fatalf("PATH env value is not an env-op directive: %q", pathVal)
	}
	if op != "prepend" {
		t.Errorf("op = %q, want prepend", op)
	}
	if len(args) != 1 || args[0] != "/install/pg/bin" {
		t.Fatalf("args = %q, want [/install/pg/bin]", args)
	}
}

func TestServiceBin_rejectsNonServiceArg(t *testing.T) {
	err := compileErr(t, `
service "pkg" "redis" {
  package = "redis@7.2.5"
  runtime {
    provision "dump" {
      env = { PATH = env::prepend(fs::service::bin("redis")) }
      cmd = "true"
    }
    cmd = ["redis-server"]
  }
}
`)
	if !strings.Contains(err.Error(), "service reference") {
		t.Fatalf("want service-reference error, got %v", err)
	}
}

func TestToolchainBin_compilesToDirective(t *testing.T) {
	// A go toolchain with no go service (the "use a go tool from another
	// project" case) — referenced by a redis provision via toolchain.go.
	af := compile(t, `
toolchain {
  go { version = "1.26.4" }
}

service "pkg" "redis" {
  package = "redis@7.2.5"
  runtime {
    provision "fmt" {
      env = { PATH = env::prepend(fs::toolchain::bin(toolchain.go)) }
      cmd = "jsonfmt ./x.json"
    }
    cmd = ["redis-server"]
  }
}
`, nil)
	svc := svcByName(af, "redis")
	if svc == nil || svc.Runtime == nil || len(svc.Runtime.Provision) != 1 {
		t.Fatalf("redis provision not resolved: %+v", svc)
	}
	resolved := SubstituteBins(svc.Runtime.Provision[0].Env["PATH"], func(kind, ref string) []string {
		if kind == "tc" && ref == "go" {
			return []string{"/install/go/bin"}
		}
		return nil
	})
	op, args, ok := ParseEnvOp(resolved)
	if !ok || op != "prepend" {
		t.Fatalf("PATH not a prepend directive: ok=%v op=%q", ok, op)
	}
	if len(args) != 1 || args[0] != "/install/go/bin" {
		t.Fatalf("args = %q, want [/install/go/bin]", args)
	}
}

func TestToolchainBin_rejectsNonToolchainArg(t *testing.T) {
	// `self` is a service object (no top-level `ready` barrier attr), so it
	// is not a toolchain reference.
	err := compileErr(t, `
service "pkg" "redis" {
  package = "redis@7.2.5"
  runtime {
    provision "fmt" {
      env = { PATH = env::prepend(fs::toolchain::bin(self)) }
      cmd = "true"
    }
    cmd = ["redis-server"]
  }
}
`)
	if !strings.Contains(err.Error(), "toolchain reference") {
		t.Fatalf("want toolchain-reference error, got %v", err)
	}
}
