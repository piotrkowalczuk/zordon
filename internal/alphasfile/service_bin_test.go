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

func TestSubstituteServiceBins_thenParse(t *testing.T) {
	// The realistic chain: env::prepend(fs::service::bin(X), "/lit") at eval
	// embeds a svcbin sentinel inside the directive; the substitution pass
	// swaps it for the resolved dir, and only then does ParseEnvOp split.
	resolve := func(id string) []string {
		if id == "service.pkg.postgres" {
			return []string{"/install/postgres/bin"}
		}
		return nil
	}
	dir := EnvOpSentinel("prepend", []string{SvcBinSentinel("service.pkg.postgres"), "/lit"})
	op, args, ok := ParseEnvOp(SubstituteServiceBins(dir, resolve))
	if !ok || op != "prepend" {
		t.Fatalf("parse: ok=%v op=%q", ok, op)
	}
	if len(args) != 2 || args[0] != "/install/postgres/bin" || args[1] != "/lit" {
		t.Fatalf("args = %q, want [/install/postgres/bin /lit]", args)
	}
}

func TestSubstituteServiceBins_unresolvedDropsToken(t *testing.T) {
	got := SubstituteServiceBins("a"+SvcBinSentinel("service.pkg.ghost")+"b", func(string) []string { return nil })
	if got != "ab" {
		t.Errorf("unresolved sentinel should vanish: got %q", got)
	}
}

func TestParseEnvOp_rejectsNonDirective(t *testing.T) {
	// Plain values, an unrelated sentinel, and a bare svcbin sentinel must
	// all read as "not a directive" so the runtime overlays them verbatim.
	for _, v := range []string{"", "/usr/bin:/bin", ArgSentinel("port"), SvcBinSentinel("service.pkg.postgres")} {
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
	resolved := SubstituteServiceBins(pathVal, func(id string) []string {
		if id == serviceID("pkg", "postgres") {
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
