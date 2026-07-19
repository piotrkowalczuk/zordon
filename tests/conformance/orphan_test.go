//go:build conformance_go

// Orphan-safety conformance: the load-bearing guarantee that a service
// never outlives the supervisor that spawned it, even when the
// supervisor is hard-killed (SIGKILL / OOM) and runs ZERO cleanup code.
//
// This drives the real stack — zordon → alpha → tommy → service — then
// SIGKILLs alpha (the OOM scenario: no signal forwarding, no shutdown
// sequence, nothing) and asserts the whole wrapped process group is
// gone. Without tommy this test would fail: today a SIGKILLed alpha
// leaves its services running until the next boot's registry reaper.
package conformance_test

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/piotrkowalczuk/zordon/internal/zordontest"
)

// TestOrphan_AlphaDeath_NoSurvivingService proves the no-orphan
// guarantee across the ways alpha can die. The interesting axis is
// whether alpha gets to run its own shutdown:
//
//   - SIGKILL  — uncatchable; the OOM analogue. Zero cleanup. tommy's
//     parent-death detectors are the ONLY thing standing between the
//     service and orphanhood.
//   - SIGQUIT  — alpha only Notify's SIGINT/SIGTERM, so SIGQUIT takes
//     the default action (terminate, dump core): again no cleanup.
//   - SIGABRT  — abort-style crash, also unhandled by alpha.
//   - SIGTERM  — alpha DOES catch this and runs its ordered shutdown
//     (which itself reaps services). Asserts tommy in the group does
//     not regress the graceful path.
//
// Every case must end with the whole wrapped process group gone and
// the port dead, with no external help (no `zordon stop`, no next-boot
// registry reaper).
func TestOrphan_AlphaDeath_NoSurvivingService(t *testing.T) {
	tommyBin := buildTommy(t)

	cases := []struct {
		name string
		sig  syscall.Signal
	}{
		{"sigkill_oom", syscall.SIGKILL},
		{"sigquit_unhandled", syscall.SIGQUIT},
		{"sigabrt_crash", syscall.SIGABRT},
		{"sigterm_graceful", syscall.SIGTERM},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// This test SIGKILLs alpha, so alpha's registry-deregister
			// never runs — a leftover row is the expected outcome (the
			// next `zordon start` would reap it in production). Nuke at
			// cleanup instead of failing AssertNoLeftovers.
			p := zordontest.NewProject(t, zordontest.WithExpectedLeftovers())
			p.CopyTree("golden/go/echo", "src/svc1")
			// Go-side free port: this subtest kills alpha, so there's no
			// settled alpha to read the picked port back from afterward.
			port := zordontest.FreePort(t)
			p.WriteFile("Alphasfile", fmt.Sprintf(`
sysenv = ["HOME", "USER", "PATH", "LANG", "TMPDIR"]
toolchain {
  go {
    version = "1.26.2"
  }
}

service "go" "svc1" {
  src {
    path = "./src/svc1"
    exe = "."
  }

  vars = { port = %d }

  runtime {
    cmd = ["${fs::bin()}/svc1", "-addr", "127.0.0.1:${self.vars.port}"]
  }

  readiness {
    http {
      path = "/"
      port = self.vars.port
    }
    period            = "200ms"
    failure_threshold = 50
  }
}
`, port))

			// Pin tommy explicitly so alpha resolves it deterministically
			// regardless of the suite's install layout.
			res := p.Zordon("start",
				"--timeout", "15m",
				"--alpha-log", p.AlphaLogPath(),
			).WithEnv("ZORDON_TOMMY_BIN", tommyBin).
				WithTimeout(16 * time.Minute).
				Run(t)
			if res.ExitCode != 0 {
				t.Fatalf("zordon start: exit %d\nstdout: %s\nstderr: %s", res.ExitCode, res.Stdout, res.Stderr)
			}

			// Surface alpha's tommy-resolution decision + spawn argv
			// no matter how the subtest ends.
			t.Cleanup(func() { dumpAlphaLog(t, p.AlphaLogPath()) })

			// Sanity: the service is up and serving before we kill anything.
			if !portServing(port, 2*time.Second) {
				t.Fatalf("service not serving on :%d after start", port)
			}

			alphaPID, groupPID := pidsFromAlphaLog(t, p.AlphaLogPath(), "svc1")
			t.Logf("alpha pid=%d, wrapped service group pgid=%d, killing alpha with %v",
				alphaPID, groupPID, tc.sig)

			if err := syscall.Kill(alphaPID, tc.sig); err != nil {
				t.Fatalf("kill alpha pid %d with %v: %v", alphaPID, tc.sig, err)
			}

			// Within a bounded window the ENTIRE wrapped group (tommy +
			// the service + anything it forked) must be gone.
			// kill(-pgid, 0) returns ESRCH only when no process remains.
			deadline := time.Now().Add(15 * time.Second)
			for time.Now().Before(deadline) {
				if groupGone(groupPID) {
					// Belt-and-braces: the port must also be dead, proving
					// it was the real service that went away, not a pid race.
					if portServing(port, 200*time.Millisecond) {
						t.Fatalf("group %d reported gone but :%d still serving", groupPID, port)
					}
					return // proven: alpha died ⇒ no orphan
				}
				time.Sleep(100 * time.Millisecond)
			}

			// Orphan: clean it up so the suite host isn't polluted, then fail.
			_ = syscall.Kill(-groupPID, syscall.SIGKILL)
			t.Fatalf("ORPHAN: wrapped service group %d still alive 15s after alpha %v",
				groupPID, tc.sig)
		})
	}
}

// TestOrphan_AlphaAlive_ServiceSurvives is the dual of
// TestOrphan_AlphaDeath_NoSurvivingService, and closes its blind spot. That
// test only asserts the wrapped group is gone AFTER alpha is killed — never
// that it SURVIVES while alpha is alive. So a broken tommy that reaps the
// service for the wrong reason (e.g. a stray CommandContext timeout that fires
// regardless of the spawner) still passes it: the group is "gone after alpha
// death" only coincidentally. Here alpha is left ALIVE, and the wrapped service
// must keep running — tommy must reap ONLY on the spawner's death, never on its
// own.
func TestOrphan_AlphaAlive_ServiceSurvives(t *testing.T) {
	tommyBin := buildTommy(t)

	// alpha keeps running past `zordon start`, so the harness's cleanup-time
	// `zordon stop` is the right teardown (no WithExpectedLeftovers).
	p := zordontest.NewProject(t)
	p.CopyTree("golden/go/echo", "src/svc1")
	port := zordontest.FreePort(t)
	p.WriteFile("Alphasfile", fmt.Sprintf(`
sysenv = ["HOME", "USER", "PATH", "LANG", "TMPDIR"]
toolchain {
  go {
    version = "1.26.2"
  }
}

service "go" "svc1" {
  src {
    path = "./src/svc1"
    exe = "."
  }

  vars = { port = %d }

  runtime {
    cmd = ["${fs::bin()}/svc1", "-addr", "127.0.0.1:${self.vars.port}"]
  }

  readiness {
    http {
      path = "/"
      port = self.vars.port
    }
    period            = "200ms"
    failure_threshold = 50
  }
}
`, port))

	res := p.Zordon("start",
		"--timeout", "15m",
		"--alpha-log", p.AlphaLogPath(),
	).WithEnv("ZORDON_TOMMY_BIN", tommyBin).
		WithTimeout(16 * time.Minute).Run(t)
	if res.ExitCode != 0 {
		t.Fatalf("zordon start: exit %d\nstdout: %s\nstderr: %s", res.ExitCode, res.Stdout, res.Stderr)
	}
	t.Cleanup(func() { dumpAlphaLog(t, p.AlphaLogPath()) })

	_, groupPID := pidsFromAlphaLog(t, p.AlphaLogPath(), "svc1")

	// alpha is deliberately LEFT ALIVE. Over a window comfortably longer than
	// any plausible premature-reap timer, the wrapped group AND its port must
	// stay up.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if groupGone(groupPID) {
			t.Fatalf("wrapped service group %d reaped while alpha is alive", groupPID)
		}
		if !portServing(port, 200*time.Millisecond) {
			t.Fatalf(":%d stopped serving while alpha is alive — tommy over-reaped the service", port)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// TestOrphan_registryReaperKillsOrphansOnNextStart proves the
// safety-net that backs the parent-watch + tommy story: if SIGKILLed
// alpha leaves a service group running (no cleanup, no deregister),
// the NEXT `zordon start` for the same fs_hash must reap that PGID
// before binding its own control socket. Without that, the orphan
// would hold the test port and the new service would fail with
// EADDRINUSE.
//
// We deliberately run alpha WITHOUT tommy so the wrapped-group
// orphan-killer isn't in play. Otherwise tommy reaps the service
// within ~250ms of alpha's death and round 2 has nothing to reap —
// the test would silently exercise tommy, not the registry.
//
// Forcing no-tommy: copy alpha into a dir with no tommy sibling
// (resolveTommyBin returns "" when neither $ZORDON_TOMMY_BIN nor a
// sibling resolves), and stamp ZORDON_TOMMY_BIN="" into the env.
func TestOrphan_registryReaperKillsOrphansOnNextStart(t *testing.T) {
	noTommyAlpha := buildAlphaNoTommySibling(t)

	p := zordontest.NewProject(t, zordontest.WithExpectedLeftovers())
	p.CopyTree("golden/go/echo", "src/svc1")
	// Same port across both rounds: this test restarts zordon, and a
	// round-2 re-eval of net::pickport() would pick a DIFFERENT port, so
	// the orphan-vs-fresh comparison needs one fixed Go-side port.
	port := zordontest.FreePort(t)

	p.WriteFile("Alphasfile", fmt.Sprintf(`
sysenv = ["HOME", "USER", "PATH", "LANG", "TMPDIR"]
toolchain {
  go {
    version = "1.26.2"
  }
}

service "go" "svc1" {
  src {
    path = "./src/svc1"
    exe = "."
  }

  vars = { port = %d }

  runtime {
    cmd = ["${fs::bin()}/svc1", "-addr", "127.0.0.1:${self.vars.port}"]
  }

  readiness {
    http {
      path = "/"
      port = self.vars.port
    }
    period            = "200ms"
    failure_threshold = 100
  }
}
`, port))

	// Round 1: bringup. alpha is the no-tommy build, so the spawn is
	// bare (alpha → service directly, Setpgid keeps the service in
	// its own group). Empty ZORDON_TOMMY_BIN belt-and-braces; alpha
	// already wouldn't find a sibling tommy in noTommyAlpha's dir.
	r1 := p.Zordon("start",
		"--alpha", noTommyAlpha,
		"--timeout", "15m",
		"--alpha-log", p.AlphaLogPath(),
	).WithEnv("ZORDON_TOMMY_BIN", "").
		WithTimeout(16 * time.Minute).Run(t)
	if r1.ExitCode != 0 {
		t.Fatalf("zordon start #1: exit %d\nstderr: %s", r1.ExitCode, r1.Stderr)
	}
	t.Cleanup(func() { dumpAlphaLog(t, p.AlphaLogPath()) })

	alphaPID, groupPID := pidsFromAlphaLog(t, p.AlphaLogPath(), "svc1")
	t.Logf("round1: alpha=%d group=%d :%d (no tommy)", alphaPID, groupPID, port)
	if !portServing(port, 2*time.Second) {
		t.Fatalf(":%d not serving after round1", port)
	}

	if err := syscall.Kill(alphaPID, syscall.SIGKILL); err != nil {
		t.Fatalf("SIGKILL alpha pid %d: %v", alphaPID, err)
	}
	// Wait for alpha itself to be reaped (becomes a zombie until
	// init/launchd collects it). 5s is generous.
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		if !pidAlive(alphaPID) {
			break
		}
	}
	if pidAlive(alphaPID) {
		t.Fatalf("alpha pid %d still alive after SIGKILL", alphaPID)
	}
	// CRITICAL precondition: the service group must survive alpha's
	// death. If it doesn't, alpha-without-tommy isn't producing the
	// orphan we want to test against — likely a code change in alpha's
	// spawn path; revisit the no-tommy assumption.
	if groupGone(groupPID) {
		t.Fatalf("service group %d gone right after alpha SIGKILL — no orphan to reap, test premise broken", groupPID)
	}
	t.Logf("orphan confirmed: group %d still serving :%d with alpha %d gone", groupPID, port, alphaPID)

	// Round 2: another `zordon start`. alpha's runAlpha calls
	// registry.ReapByFsHash BEFORE control.Listen, so the orphan PGID
	// must be killed before round-2 alpha even binds. The fact that
	// this start succeeds (no EADDRINUSE, fresh service serving) is
	// the assertion.
	r2 := p.Zordon("start",
		"--alpha", noTommyAlpha,
		"--timeout", "15m",
		"--alpha-log", p.AlphaLogPath(),
	).WithEnv("ZORDON_TOMMY_BIN", "").
		WithTimeout(16 * time.Minute).Run(t)
	if r2.ExitCode != 0 {
		t.Fatalf("zordon start #2 (post-orphan): exit %d\nstderr: %s — registry reaper likely failed to kill PGID %d",
			r2.ExitCode, r2.Stderr, groupPID)
	}

	// Round 1's group must be gone now.
	if !groupGone(groupPID) {
		t.Fatalf("round1 service group %d still alive after round2 start — registry reaper did not fire", groupPID)
	}
	// And the round-2 service must be serving.
	if !portServing(port, 2*time.Second) {
		t.Fatalf(":%d not serving after round2 start", port)
	}
}

// buildAlphaNoTommySibling compiles cmd/alpha into a fresh temp dir
// where no `tommy` exists alongside it. alpha's resolveTommyBin then
// returns "" and services spawn unwrapped — the production fallback
// that the registry reaper is explicitly designed to backstop.
func buildAlphaNoTommySibling(t *testing.T) string {
	t.Helper()
	root := moduleRoot(t)
	dir := t.TempDir()
	out := dir + "/alpha"
	cmd := exec.Command("go", "build", "-o", out, "./cmd/alpha")
	cmd.Dir = root
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build alpha: %v\n%s", err, b)
	}
	return out
}

// buildTommy compiles cmd/tommy into a temp path so the test pins it
// via $ZORDON_TOMMY_BIN no matter how the rest of the suite is built.
func buildTommy(t *testing.T) string {
	t.Helper()
	root := moduleRoot(t)
	bin := filepath.Join(t.TempDir(), "tommy")
	cmd := exec.Command("go", "build", "-o", bin, "./cmd/tommy")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build tommy: %v\n%s", err, out)
	}
	return bin
}

// moduleRoot walks up from the test's cwd to the dir holding go.mod.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("go.mod not found above %s", dir)
		}
		dir = parent
	}
}

var (
	reAlphaStart = regexp.MustCompile(`starting pid=(\d+)`)
	reSvcStart   = regexp.MustCompile(`started svc1 pid=(\d+)`)
)

// pidsFromAlphaLog extracts alpha's own pid and the wrapped service's
// process-group pid (alpha logs cmd.Process.Pid for the spawned
// process — that's tommy, the group leader, since alpha sets Setpgid).
func pidsFromAlphaLog(t *testing.T, logPath, svc string) (alphaPID, groupPID int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(logPath)
		if err == nil {
			if m := reAlphaStart.FindSubmatch(b); m != nil {
				alphaPID, _ = strconv.Atoi(string(m[1]))
			}
			if m := reSvcStart.FindSubmatch(b); m != nil {
				groupPID, _ = strconv.Atoi(string(m[1]))
			}
			if alphaPID > 0 && groupPID > 0 {
				return alphaPID, groupPID
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("could not parse alpha pid / %s pid from %s", svc, logPath)
	return 0, 0
}

// dumpAlphaLog prints the lines that reveal whether tommy was
// interposed and with what argv — the first thing to check on failure.
func dumpAlphaLog(t *testing.T, logPath string) {
	t.Helper()
	b, err := os.ReadFile(logPath)
	if err != nil {
		t.Logf("alpha.log unreadable: %v", err)
		return
	}
	for ln := range strings.SplitSeq(string(b), "\n") {
		if strings.Contains(ln, "tommy") || strings.Contains(ln, "started svc1") ||
			strings.Contains(ln, "starting pid=") {
			t.Logf("alpha.log| %s", ln)
		}
	}
}

// groupGone reports whether no process remains in pgid's group.
// Signalling a negative pid targets the whole group; ESRCH means empty.
func groupGone(pgid int) bool {
	err := syscall.Kill(-pgid, 0)
	return err == syscall.ESRCH
}

// portServing reports whether something accepts TCP on the port within d.
func portServing(port int, d time.Duration) bool {
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), d)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}
