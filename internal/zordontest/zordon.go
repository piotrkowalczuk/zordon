package zordontest

import (
	"bytes"
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// ZordonCmd is a builder for one `zordon <args...>` invocation.
// Chain `.WithEnv`/`.WithTimeout` to configure, terminate with
// `.Run(t)` to execute.
type ZordonCmd struct {
	p       *Project
	args    []string
	extraEnv map[string]string
	timeout time.Duration
}

// Zordon returns a builder for the given subcommand (start / stop /
// status / sudo / wt / get). Args after the subcommand are passed
// through verbatim.
func (p *Project) Zordon(args ...string) *ZordonCmd {
	return &ZordonCmd{
		p:       p,
		args:    args,
		timeout: 60 * time.Second, // generous default; bringup can take a while on first run
	}
}

// WithEnv sets KEY=VALUE in the invocation's env, overriding any
// inherited value from os.Environ(). Use for ZORDON_HOME overrides
// per-call (rare; usually the project-level home covers it).
func (z *ZordonCmd) WithEnv(key, value string) *ZordonCmd {
	if z.extraEnv == nil {
		z.extraEnv = map[string]string{}
	}
	z.extraEnv[key] = value
	return z
}

// WithTimeout overrides the default per-invocation timeout. Set
// generously — bringup that compiles a fresh service can take
// minutes. The harness kills the zordon process on timeout to keep
// the test from hanging the suite, but doesn't propagate to alpha
// (alpha is its own process and would survive).
func (z *ZordonCmd) WithTimeout(d time.Duration) *ZordonCmd {
	z.timeout = d
	return z
}

// ZordonResult is the captured outcome of one invocation.
type ZordonResult struct {
	ExitCode int
	Stdout   string
	Stderr   string
}

// Run executes the configured invocation, capturing stdout/stderr.
// Fails the test if the binary can't even be spawned (path wrong,
// permission denied) — non-zero exit codes are NOT fatal; the test
// inspects ZordonResult.ExitCode and decides.
func (z *ZordonCmd) Run(t *testing.T) ZordonResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), z.timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, z.p.binZ, z.args...)
	cmd.Dir = z.p.root
	cmd.Env = z.envWithOverrides()

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	result := ZordonResult{
		Stdout: stdout.String(),
		Stderr: stderr.String(),
	}
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			result.ExitCode = ee.ExitCode()
		} else {
			// Could not spawn (PATH wrong, etc.) — that's a harness
			// failure, not a zordon failure.
			t.Fatalf("zordon %s: spawn failed: %v\nstdout: %s\nstderr: %s",
				strings.Join(z.args, " "), err, result.Stdout, result.Stderr)
		}
	}
	return result
}

func (z *ZordonCmd) envWithOverrides() []string {
	env := z.p.env()
	for k, v := range z.extraEnv {
		env = setEnv(env, k, v)
	}
	return env
}
