//go:build conformance_java

package conformance_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/piotrkowalczuk/zordon/internal/zordontest"
)

// Java conformance: one test per `service "java" { ... }` shape, plus a
// table over the build-tool axis (Maven vs Gradle) that go/rust don't
// have. Fixtures live in golden/java/echo-maven (./mvnw) and
// golden/java/echo-gradle (./gradlew); both build the same
// JDK-only HTTP echo and speak the shared wire shape, so mustDecodeEcho
// (go_test.go) decodes them.
//
// The default build is wrapper-first: ./mvnw / ./gradlew self-provision
// their build tool (cached under HOME's ~/.m2 / ~/.gradle across tests),
// so only a JDK is pinned via mise. First run downloads the Maven/Gradle
// distributions; subsequent tests in the same run reuse them.
const (
	javaVersion        = "temurin-21.0.5+11.0.LTS"
	javaRuntimeVersion = "21.0.5" // what System.getProperty("java.version") reports
)

// Canonical Maven shape: src + default ./mvnw build + inferred
// `java -jar target/echo.jar` + HTTP probe.
func TestJavaService_srcDefault_maven(t *testing.T) {
	p := zordontest.NewProject(t)
	p.CopyTree("golden/java/echo-maven", "src/echo")
	p.WriteFile("Alphasfile", javaAlphasfile("echo", `
  src { path = "./src/echo" }

  vars = { port = net::pickport() }
  env  = { PORT = "${self.vars.port}" }

  readiness {
    http {
      path = "/"
      port = self.vars.port
    }
    period            = "500ms"
    failure_threshold = 120
  }`))

	mustStart(t, p)
	port := p.Get(t, "service.java.echo.vars.port").Int()
	mustGetJavaEcho(t, port)
}

// Canonical Gradle shape: same, from golden/java/echo-gradle (./gradlew,
// build/libs/echo.jar).
func TestJavaService_srcDefault_gradle(t *testing.T) {
	p := zordontest.NewProject(t)
	p.CopyTree("golden/java/echo-gradle", "src/echo")
	p.WriteFile("Alphasfile", javaAlphasfile("echo", `
  src { path = "./src/echo" }

  vars = { port = net::pickport() }
  env  = { PORT = "${self.vars.port}" }

  readiness {
    http {
      path = "/"
      port = self.vars.port
    }
    period            = "500ms"
    failure_threshold = 120
  }`))

	mustStart(t, p)
	port := p.Get(t, "service.java.echo.vars.port").Int()
	mustGetJavaEcho(t, port)
}

// Explicit `build { cmd }` overrides the wrapper-first default; still
// produces target/echo.jar, which inference then runs.
func TestJavaService_srcExplicitBuildCmd(t *testing.T) {
	p := zordontest.NewProject(t)
	p.CopyTree("golden/java/echo-maven", "src/echo")
	p.WriteFile("Alphasfile", javaAlphasfile("echo", `
  src { path = "./src/echo" }

  build { cmd = ["./mvnw", "-q", "-B", "-DskipTests", "package"] }

  vars = { port = net::pickport() }
  env  = { PORT = "${self.vars.port}" }

  readiness {
    http {
      path = "/"
      port = self.vars.port
    }
    period            = "500ms"
    failure_threshold = 120
  }`))

	mustStart(t, p)
	port := p.Get(t, "service.java.echo.vars.port").Int()
	mustGetJavaEcho(t, port)
}

// Explicit `runtime { cmd }` bypasses jar inference; a regression that
// let inference win would strip the -addr argv.
func TestJavaService_srcExplicitRuntimeCmd(t *testing.T) {
	p := zordontest.NewProject(t)
	p.CopyTree("golden/java/echo-maven", "src/echo")
	p.WriteFile("Alphasfile", javaAlphasfile("echo", `
  src { path = "./src/echo" }

  vars = { port = net::pickport() }

  runtime {
    cmd = ["java", "-jar", "target/echo.jar", "-addr", "127.0.0.1:${self.vars.port}"]
  }

  readiness {
    http {
      path = "/"
      port = self.vars.port
    }
    period            = "500ms"
    failure_threshold = 120
  }`))

	mustStart(t, p)
	port := p.Get(t, "service.java.echo.vars.port").Int()
	mustGetJavaEcho(t, port)
}

// No readiness block → stabilization timer fallback.
func TestJavaService_noReadinessUsesStabilization(t *testing.T) {
	p := zordontest.NewProject(t)
	p.CopyTree("golden/java/echo-maven", "src/echo")
	p.WriteFile("Alphasfile", javaAlphasfile("echo", `
  src { path = "./src/echo" }

  vars = { port = net::pickport() }
  env  = { PORT = "${self.vars.port}" }`))

	mustStart(t, p)
	port := p.Get(t, "service.java.echo.vars.port").Int()
	mustGetJavaEcho(t, port)
}

// Service `env {}` must reach the JVM process; echo mirrors it back.
func TestJavaService_envBlockReachesRuntime(t *testing.T) {
	p := zordontest.NewProject(t)
	p.CopyTree("golden/java/echo-maven", "src/echo")
	p.WriteFile("Alphasfile", javaAlphasfile("echo", `
  src { path = "./src/echo" }

  vars = { port = net::pickport() }
  env  = {
    PORT     = "${self.vars.port}"
    GREETING = "hello-from-env-block"
  }

  readiness {
    http {
      path = "/"
      port = self.vars.port
    }
    period            = "500ms"
    failure_threshold = 120
  }`))

	mustStart(t, p)
	port := p.Get(t, "service.java.echo.vars.port").Int()
	echo := mustDecodeEcho(t, port)
	if got := echo.Env["GREETING"]; got != "hello-from-env-block" {
		t.Errorf("GREETING in service env = %q; want %q", got, "hello-from-env-block")
	}
}

// Build-tool auto-detection: Maven picks ./mvnw, Gradle picks ./gradlew,
// and both end up running `java -jar` on the produced jar.
func TestJavaService_buildToolDetect_table(t *testing.T) {
	cases := map[string]struct {
		goldenDir string
		wantBuild string // build-cmd substring in alpha log
	}{
		"maven-mvnw":     {goldenDir: "golden/java/echo-maven", wantBuild: "mvnw"},
		"gradle-gradlew": {goldenDir: "golden/java/echo-gradle", wantBuild: "gradlew"},
	}

	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			p := zordontest.NewProject(t)
			p.CopyTree(c.goldenDir, "src/echo")
			port := zordontest.FreePort(t)
			p.WriteFile("Alphasfile", javaAlphasfile("echo", fmt.Sprintf(`
  src { path = "./src/echo" }

  vars = { port = %d }
  env  = { PORT = "${self.vars.port}" }

  readiness {
    http {
      path = "/"
      port = self.vars.port
    }
    period            = "500ms"
    failure_threshold = 120
  }`, port)))

			mustStart(t, p)
			mustGetJavaEcho(t, port)

			log := p.AlphaLog()
			if !log.Contains(c.wantBuild) {
				t.Errorf("alpha log lacks %q — build-tool detection picked the wrong wrapper", c.wantBuild)
			}
			if !log.Contains("java -jar") {
				t.Errorf("alpha log lacks %q — run inference didn't produce java -jar", "java -jar")
			}
		})
	}
}

// git source requires a network clone from a supported host; the
// spring-petclinic e2e example covers it. No offline path here.
func TestJavaService_gitSource(t *testing.T) {
	t.Skip("git source requires a network clone from a supported host; covered by examples/java (spring-petclinic)")
}

// java has no use-only mode (like ruby/nodejs) — see docs/services/java.md.
func TestJavaService_useOnly(t *testing.T) {
	t.Skip("java has no use-only mode; workspace-only (src/git)")
}

// javaAlphasfile wraps a service body in the shared toolchain + sysenv
// preamble. HOME is whitelisted so the Maven/Gradle wrapper can reach
// its ~/.m2 / ~/.gradle caches.
func javaAlphasfile(name, body string) string {
	return fmt.Sprintf(`
sysenv = ["HOME", "USER", "PATH", "LANG", "TMPDIR"]
toolchain {
  java {
    version = %q
  }
}

service "java" %q {
%s
}
`, javaVersion, name, body)
}

// mustGetJavaEcho confirms reachability AND that runtime_version reports
// the mise-pinned JDK.
func mustGetJavaEcho(t *testing.T, port int) {
	t.Helper()
	echo := mustDecodeEcho(t, port)
	if !strings.Contains(echo.RuntimeVersion, javaRuntimeVersion) {
		t.Errorf("runtime_version = %q; want it to contain pinned JDK %s", echo.RuntimeVersion, javaRuntimeVersion)
	}
}
