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
		port int
	}{
		{"sigkill_oom", syscall.SIGKILL, 27931},
		{"sigquit_unhandled", syscall.SIGQUIT, 27932},
		{"sigabrt_crash", syscall.SIGABRT, 27933},
		{"sigterm_graceful", syscall.SIGTERM, 27934},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// This test SIGKILLs alpha, so alpha's registry-deregister
			// never runs — a leftover row is the expected outcome (the
			// next `zordon start` would reap it in production). Nuke at
			// cleanup instead of failing AssertNoLeftovers.
			p := zordontest.NewProject(t, zordontest.WithExpectedLeftovers())
			p.CopyTree("golden/go/echo", "src/svc1")
			p.WriteFile("Alphasfile", zordontest.GoToolchainHCL()+fmt.Sprintf(`
sysenv = ["HOME", "USER", "PATH", "LANG", "TMPDIR"]
service "go" "svc1" {
  src = "./src/svc1"
  exe = "."

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
`, tc.port))

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
			if !portServing(tc.port, 2*time.Second) {
				t.Fatalf("service not serving on :%d after start", tc.port)
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
					if portServing(tc.port, 200*time.Millisecond) {
						t.Fatalf("group %d reported gone but :%d still serving", groupPID, tc.port)
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
	for _, ln := range strings.Split(string(b), "\n") {
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
