package zordonhome

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolve_overrideWins(t *testing.T) {
	t.Setenv("ZORDON_HOME", "/from/env")
	if got := Resolve("/from/flag"); got != "/from/flag" {
		t.Errorf("Resolve(/from/flag) = %q; want explicit override to win", got)
	}
}

func TestResolve_envWinsOverDefault(t *testing.T) {
	t.Setenv("ZORDON_HOME", "/from/env")
	if got := Resolve(""); got != "/from/env" {
		t.Errorf("Resolve with env set = %q; want /from/env", got)
	}
}

// REGRESSION: with neither override nor env, fall back to <HOME>/.zordon
// so production CLI invocations (no harness, no flag) keep working.
func TestResolve_defaultsToUserHomeZordon(t *testing.T) {
	// Setenv with empty string ⇒ Getenv returns "" ⇒ falls through to default.
	t.Setenv("ZORDON_HOME", "")

	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("UserHomeDir unavailable on this platform")
	}
	want := filepath.Join(home, ".zordon")
	if got := Resolve(""); got != want {
		t.Errorf("Resolve() = %q; want %q", got, want)
	}
}
