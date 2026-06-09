package alphasfile

import (
	"strings"
	"testing"
)

func compileErr(t *testing.T, src string) error {
	t.Helper()
	_, err := Compile("test.hcl", []byte(src), testInv(), nil, testCfgHash, TestConfig{})
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	return err
}

func TestResolvePkg_objectCoordinate(t *testing.T) {
	af := compile(t, `
service "pkg" "etcd" {
  package = { name = "etcd-io/etcd", backend = "aqua", version = "3.5.17" }
  vars = { port = net::pickport() }
  runtime { cmd = ["etcd", "--listen-client-urls", "http://127.0.0.1:${self.vars.port}"] }
  readiness {
    tcp { port = self.vars.port }
  }
}
`, nil)
	svc := svcByName(af, "etcd")
	if svc == nil {
		t.Fatal("service etcd not resolved")
	}
	if svc.Package != nil {
		t.Errorf("pkg service must not set Package, got %+v", svc.Package)
	}
	if svc.Pkg == nil {
		t.Fatal("svc.Pkg is nil")
	}
	// backend folds into the mise tool ref: <backend>:<name>.
	if svc.Pkg.Name != "aqua:etcd-io/etcd" {
		t.Errorf("Pkg.Name = %q, want aqua:etcd-io/etcd", svc.Pkg.Name)
	}
	if svc.Pkg.Version != "3.5.17" {
		t.Errorf("Pkg.Version = %q, want 3.5.17", svc.Pkg.Version)
	}
	if svc.Runtime.Readiness == nil || svc.Runtime.Readiness.TCP == nil {
		t.Fatalf("expected TCP readiness, got %+v", svc.Runtime.Readiness)
	}
	if svc.Runtime.Readiness.TCP.Port <= 0 {
		t.Errorf("TCP port not resolved: %d", svc.Runtime.Readiness.TCP.Port)
	}
}

func TestResolvePkg_stringCoordinate(t *testing.T) {
	af := compile(t, `
service "pkg" "redis" {
  package = "redis@7.2.5"
  runtime { cmd = ["redis-server"] }
}
`, nil)
	svc := svcByName(af, "redis")
	if svc == nil || svc.Pkg == nil || svc.Pkg.Name != "redis" || svc.Pkg.Version != "7.2.5" {
		t.Fatalf("Pkg = %+v, want {redis 7.2.5}", svc.Pkg)
	}
}

func TestResolvePkg_backendStringCoordinate(t *testing.T) {
	af := compile(t, `
service "pkg" "etcd" {
  package = "aqua:etcd-io/etcd@3.5.17"
  runtime { cmd = ["etcd"] }
}
`, nil)
	svc := svcByName(af, "etcd")
	if svc == nil || svc.Pkg == nil || svc.Pkg.Name != "aqua:etcd-io/etcd" || svc.Pkg.Version != "3.5.17" {
		t.Fatalf("Pkg = %+v, want {aqua:etcd-io/etcd 3.5.17}", svc.Pkg)
	}
}

func TestResolvePkg_requiresRuntimeCmd(t *testing.T) {
	err := compileErr(t, `
service "pkg" "redis" {
  package = "redis@7.2.5"
}
`)
	if !strings.Contains(err.Error(), "runtime") {
		t.Fatalf("want runtime.cmd-required error, got %v", err)
	}
}

func TestResolvePkg_requiresVersion(t *testing.T) {
	err := compileErr(t, `
service "pkg" "redis" {
  package = "redis"
  runtime { cmd = ["redis-server"] }
}
`)
	if !strings.Contains(err.Error(), "version") {
		t.Fatalf("want version-required error, got %v", err)
	}
}

func TestResolvePkg_objectRequiresVersion(t *testing.T) {
	err := compileErr(t, `
service "pkg" "redis" {
  package = { name = "redis" }
  runtime { cmd = ["redis-server"] }
}
`)
	if !strings.Contains(err.Error(), "version") {
		t.Fatalf("want version-required error, got %v", err)
	}
}

func TestResolvePkg_rejectsSource(t *testing.T) {
	err := compileErr(t, `
service "pkg" "redis" {
  package = "redis@7.2.5"
  git { url = "github.com/x/y" }
  runtime { cmd = ["redis-server"] }
}
`)
	if !strings.Contains(err.Error(), "no source build") {
		t.Fatalf("want source-rejection error, got %v", err)
	}
}

func TestResolveGo_packageRejectsObject(t *testing.T) {
	err := compileErr(t, `
service "go" "api" {
  package = { name = "x", version = "1" }
  runtime { cmd = ["api"] }
}
`)
	if !strings.Contains(err.Error(), "must be a string") {
		t.Fatalf("want go-package-string error, got %v", err)
	}
}
