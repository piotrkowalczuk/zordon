package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// javaBuildCmd is wrapper-first: it prefers the committed ./mvnw / ./gradlew
// so the project's pinned build-tool version wins, and fails loudly (rather
// than silently no-op'ing) when a build file is present without its wrapper.
func TestJavaBuildCmd(t *testing.T) {
	cases := map[string]struct {
		files []string // marker files to touch in the project root
		want  string   // exact command (for the happy paths)
		errIn string   // substring the failing snippet must contain
	}{
		"maven-wrapper":     {files: []string{"pom.xml", "mvnw"}, want: "./mvnw -q -B -DskipTests package"},
		"gradle-wrapper":    {files: []string{"build.gradle", "gradlew"}, want: "./gradlew --console=plain --no-daemon -x test build"},
		"gradle-kts":        {files: []string{"build.gradle.kts", "gradlew"}, want: "./gradlew --console=plain --no-daemon -x test build"},
		"maven-no-wrapper":  {files: []string{"pom.xml"}, errIn: "no ./mvnw wrapper"},
		"gradle-no-wrapper": {files: []string{"build.gradle"}, errIn: "no ./gradlew wrapper"},
		"no-build-file":     {files: nil, errIn: "no pom.xml or build.gradle"},
	}

	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			root := t.TempDir()
			for _, f := range c.files {
				touch(t, filepath.Join(root, f))
			}
			got := javaBuildCmd(root)
			if c.want != "" {
				if got != c.want {
					t.Errorf("javaBuildCmd = %q; want %q", got, c.want)
				}
				return
			}
			if !strings.Contains(got, c.errIn) {
				t.Errorf("javaBuildCmd = %q; want a failing snippet containing %q", got, c.errIn)
			}
			if !strings.Contains(got, "exit 1") {
				t.Errorf("javaBuildCmd = %q; want the snippet to exit non-zero", got)
			}
		})
	}
}

// inferJavaRunCmd finds the single runnable jar the build produced and runs
// `java -jar <jar>`, excluding the non-runnable siblings Spring Boot and the
// javadoc/sources plugins leave behind.
func TestInferJavaRunCmd(t *testing.T) {
	t.Run("maven-single", func(t *testing.T) {
		root := t.TempDir()
		touch(t, filepath.Join(root, "pom.xml"))
		mkJar(t, root, "target", "echo.jar")
		assertRunsJar(t, root, filepath.Join(root, "target", "echo.jar"))
	})

	t.Run("gradle-single", func(t *testing.T) {
		root := t.TempDir()
		touch(t, filepath.Join(root, "build.gradle"))
		mkJar(t, root, filepath.Join("build", "libs"), "echo.jar")
		assertRunsJar(t, root, filepath.Join(root, "build", "libs", "echo.jar"))
	})

	t.Run("maven-excludes-sources-javadoc-original", func(t *testing.T) {
		root := t.TempDir()
		touch(t, filepath.Join(root, "pom.xml"))
		for _, n := range []string{"app.jar", "app-sources.jar", "app-javadoc.jar", "app.jar.original"} {
			mkJar(t, root, "target", n)
		}
		assertRunsJar(t, root, filepath.Join(root, "target", "app.jar"))
	})

	t.Run("gradle-excludes-plain", func(t *testing.T) {
		root := t.TempDir()
		touch(t, filepath.Join(root, "build.gradle"))
		mkJar(t, root, filepath.Join("build", "libs"), "app.jar")
		mkJar(t, root, filepath.Join("build", "libs"), "app-plain.jar")
		assertRunsJar(t, root, filepath.Join(root, "build", "libs", "app.jar"))
	})

	t.Run("zero-jars", func(t *testing.T) {
		root := t.TempDir()
		touch(t, filepath.Join(root, "pom.xml"))
		if err := os.MkdirAll(filepath.Join(root, "target"), 0o755); err != nil {
			t.Fatal(err)
		}
		assertInferErr(t, root, "no runnable jar")
	})

	t.Run("multiple-jars", func(t *testing.T) {
		root := t.TempDir()
		touch(t, filepath.Join(root, "pom.xml"))
		mkJar(t, root, "target", "a.jar")
		mkJar(t, root, "target", "b.jar")
		assertInferErr(t, root, "multiple jars")
	})

	t.Run("no-build-file", func(t *testing.T) {
		assertInferErr(t, t.TempDir(), "no pom.xml or build.gradle")
	})

	t.Run("missing-output-dir", func(t *testing.T) {
		root := t.TempDir()
		touch(t, filepath.Join(root, "pom.xml"))
		assertInferErr(t, root, "read build output")
	})
}

func assertRunsJar(t *testing.T, root, wantJar string) {
	t.Helper()
	argv, err := inferJavaRunCmd(root)
	if err != nil {
		t.Fatalf("inferJavaRunCmd: %v", err)
	}
	want := []string{"java", "-jar", wantJar}
	if len(argv) != 3 || argv[0] != want[0] || argv[1] != want[1] || argv[2] != want[2] {
		t.Errorf("inferJavaRunCmd = %v; want %v", argv, want)
	}
}

func assertInferErr(t *testing.T, root, errIn string) {
	t.Helper()
	_, err := inferJavaRunCmd(root)
	if err == nil {
		t.Fatalf("inferJavaRunCmd(%s) = nil error; want one containing %q", root, errIn)
	}
	if !strings.Contains(err.Error(), errIn) {
		t.Errorf("inferJavaRunCmd error = %q; want it to contain %q", err, errIn)
	}
}

func touch(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
}

func mkJar(t *testing.T, root, subdir, name string) {
	t.Helper()
	dir := filepath.Join(root, subdir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), nil, 0o644); err != nil {
		t.Fatal(err)
	}
}
