//go:build conformance_java

package conformance_test

import (
	"path/filepath"
	"strings"
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
		"JAVA_HOME":         poisonSentinel + "/jdk",
		"JDK_HOME":          poisonSentinel + "/jdk",
		"JAVA_TOOL_OPTIONS": "-Xmx1k",
		"_JAVA_OPTIONS":     "-Xmx1k",
		"JDK_JAVA_OPTIONS":  "-Xmx1k",
		"CLASSPATH":         poisonSentinel + "/poison.jar",
		"MAVEN_OPTS":        "-Xmx1k",
		"MAVEN_ARGS":        "--poison",
		"MAVEN_HOME":        poisonSentinel + "/maven",
		"M2_HOME":           poisonSentinel + "/maven",
		"MAVEN_USER_HOME":   poisonSentinel + "/m2",
		"SDKMAN_DIR":        poisonSentinel + "/sdkman",
		"MISE_DATA_DIR":     poisonSentinel + "/mise",
	})

	port := p.Get(t, "service.java.echo.vars.port").Int()
	echo := mustDecodeEcho(t, port)
	assertNoPoison(t, echo.Env, "JDK_HOME", "JAVA_TOOL_OPTIONS", "_JAVA_OPTIONS", "JDK_JAVA_OPTIONS", "CLASSPATH", "MAVEN_OPTS", "MAVEN_HOME", "M2_HOME", "SDKMAN_DIR", "MISE_DATA_DIR")
	if !strings.HasPrefix(echo.Env["JAVA_HOME"], filepath.Join(p.Home(), "toolchain", "installs", "java")) {
		t.Errorf("JAVA_HOME = %q, want the mise install", echo.Env["JAVA_HOME"])
	}
	assertUnderZordonHome(t, p, echo.Env, "MAVEN_USER_HOME")
	assertUnderZordonHome(t, p, echo.Env, "GRADLE_USER_HOME")
	if echo.Env["MAVEN_SKIP_RC"] != "1" {
		t.Errorf("MAVEN_SKIP_RC = %q, want 1 (~/.mavenrc must be skipped)", echo.Env["MAVEN_SKIP_RC"])
	}
	assertHomeUntouched(t, home, ".m2/repository", ".m2/wrapper")
}

// Same for Gradle: ~/.gradle/gradle.properties and ~/.gradle/init.d are
// read from GRADLE_USER_HOME, so a proxy to a dead port there sinks the
// wrapper's distribution download and every dependency fetch, and an init
// script — the way developers inject plugins and repository mirrors into
// every build — aborts it outright, if either is read.
func TestJavaService_hermetic_gradle_hostConfigIgnored(t *testing.T) {
	home := poisonedHome(t,
		homeFile{".gradle/gradle.properties", "systemProp.http.proxyHost=127.0.0.1\nsystemProp.http.proxyPort=1\nsystemProp.https.proxyHost=127.0.0.1\nsystemProp.https.proxyPort=1\norg.gradle.jvmargs=-Xmx1k\n"},
		homeFile{".gradle/init.d/poison.gradle", "throw new GradleException(\"poisoned ~/.gradle/init.d was read\")\n"},
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
		"GRADLE_OPTS":      "-Xmx1k",
		"GRADLE_USER_HOME": poisonSentinel + "/gradle",
		"GRADLE_HOME":      poisonSentinel + "/gradle-dist",
		"JAVA_HOME":        poisonSentinel + "/jdk",
		"_JAVA_OPTIONS":    "-Xmx1k",
	})

	port := p.Get(t, "service.java.echo.vars.port").Int()
	echo := mustDecodeEcho(t, port)
	assertNoPoison(t, echo.Env, "GRADLE_OPTS", "GRADLE_HOME", "_JAVA_OPTIONS")
	assertUnderZordonHome(t, p, echo.Env, "GRADLE_USER_HOME")
	assertHomeUntouched(t, home, ".gradle/caches", ".gradle/wrapper", ".gradle/daemon")
}
