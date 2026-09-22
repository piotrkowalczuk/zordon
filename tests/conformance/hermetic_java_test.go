//go:build conformance_java

package conformance_test

import (
	"testing"

	"github.com/piotrkowalczuk/zordon/internal/zordontest"
)

// Maven's user settings.xml (mirrors, servers, proxies) and ~/.mavenrc
// (MAVEN_OPTS) both live in HOME and both used to shape every zordon Maven
// build. Poisoned: a mirror for everything pointing at a dead port, so a
// dependency fetch through it fails, and a 1k heap that no JVM starts
// with. A green ./mvnw build proves Maven ran with zordon's own settings
// and repository, and left ~/.m2 alone.
func TestJavaService_hermetic_maven_hostConfigIgnored(t *testing.T) {
	home := poisonedHome(t,
		homeFile{".m2/settings.xml", `<settings>
  <mirrors>
    <mirror><id>poison</id><mirrorOf>*</mirrorOf><url>http://127.0.0.1:1/poison</url></mirror>
  </mirrors>
</settings>
`},
		homeFile{".mavenrc", "MAVEN_OPTS=-Xmx1k\n"},
	)

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

	startPoisoned(t, p, home, map[string]string{
		"JAVA_TOOL_OPTIONS": "-Xmx1k",
		"MAVEN_OPTS":        "-Xmx1k",
	})

	port := p.Get(t, "service.java.echo.vars.port").Int()
	echo := mustDecodeEcho(t, port)
	assertNoPoison(t, echo.Env, "JAVA_TOOL_OPTIONS", "MAVEN_OPTS")
	assertUnderZordonHome(t, p, echo.Env, "MAVEN_USER_HOME")
	assertUnderZordonHome(t, p, echo.Env, "GRADLE_USER_HOME")
	if echo.Env["MAVEN_SKIP_RC"] != "1" {
		t.Errorf("MAVEN_SKIP_RC = %q, want 1 (~/.mavenrc must be skipped)", echo.Env["MAVEN_SKIP_RC"])
	}
	assertHomeUntouched(t, home, ".m2/repository", ".m2/wrapper")
}

// Same for Gradle: ~/.gradle/gradle.properties is read from GRADLE_USER_HOME,
// so a proxy to a dead port there sinks the wrapper's distribution download
// and every dependency fetch if it is read.
func TestJavaService_hermetic_gradle_hostConfigIgnored(t *testing.T) {
	home := poisonedHome(t,
		homeFile{".gradle/gradle.properties", "systemProp.http.proxyHost=127.0.0.1\nsystemProp.http.proxyPort=1\nsystemProp.https.proxyHost=127.0.0.1\nsystemProp.https.proxyPort=1\n"},
	)

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

	startPoisoned(t, p, home, map[string]string{
		"GRADLE_OPTS": "-Xmx1k",
	})

	port := p.Get(t, "service.java.echo.vars.port").Int()
	echo := mustDecodeEcho(t, port)
	assertNoPoison(t, echo.Env, "GRADLE_OPTS")
	assertUnderZordonHome(t, p, echo.Env, "GRADLE_USER_HOME")
	assertHomeUntouched(t, home, ".gradle/caches", ".gradle/wrapper", ".gradle/daemon")
}
