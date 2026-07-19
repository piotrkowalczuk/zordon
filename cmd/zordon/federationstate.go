package main

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"time"

	"github.com/piotrkowalczuk/zordon/internal/alphasfile"
	"github.com/piotrkowalczuk/zordon/internal/control"
	"github.com/piotrkowalczuk/zordon/internal/invocation"
	"github.com/piotrkowalczuk/zordon/internal/protocol"
	"github.com/piotrkowalczuk/zordon/internal/zfs"
	"github.com/piotrkowalczuk/zordon/internal/zlog"
)

// ChainLevel is one Alphasfile in the federation chain as discovered: its
// path, raw bytes, per-invocation identity, and whether it is the leaf
// (invocation) level. Pure discovery facts — the dynamic per-level bits
// (parent context, cfg hash) depend on the descending accumulator and are
// computed by the walk, not here.
type ChainLevel struct {
	afPath string
	bytes  []byte
	inv    *invocation.InvocationState
	isLeaf bool
}

func (l ChainLevel) Path() string                            { return l.afPath }
func (l ChainLevel) Bytes() []byte                           { return l.bytes }
func (l ChainLevel) Invocation() *invocation.InvocationState { return l.inv }

// IsLeaf reports whether this is the invocation (leaf) level — the one the
// user ran `zordon start` in. Leaf-ness is a chain-position fact, NOT a
// directory fact, so it lives here and not on InvocationState (which is pure
// directory facts).
func (l ChainLevel) IsLeaf() bool { return l.isLeaf }

// FederationState is the discovered chain plus each level's identity, probed
// once (walkUp → read bytes → build InvocationState). Both runStart and
// walkChain iterate it, so discovery + identity construction lives in one
// place instead of being duplicated. Zero HCL, zero eval — just environment
// probing.
type FederationState struct {
	levels []ChainLevel
}

// NewFederationState discovers the chain root→leaf from the cwd, reads each
// Alphasfile, and builds its InvocationState (leaf from cwd; parents rooted at
// their own dir).
func NewFederationState(zordonHome string) (*FederationState, error) {
	chain, invFile, err := discoverChain(zordonHome)
	if err != nil {
		return nil, err
	}
	cwd, err := zfs.Getwd()
	if err != nil {
		return nil, err
	}
	levels := make([]ChainLevel, 0, len(chain))
	for _, afPath := range chain {
		raw, err := zfs.Read(afPath)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", afPath, err)
		}
		isLeaf := afPath == invFile
		var inv *invocation.InvocationState
		if isLeaf {
			inv, err = invocation.NewInvocationState(cwd)
		} else {
			inv, err = invocation.NewInvocationStateAt(filepath.Dir(afPath))
		}
		if err != nil {
			return nil, err
		}
		levels = append(levels, ChainLevel{afPath: afPath, bytes: raw, inv: inv, isLeaf: isLeaf})
	}
	return &FederationState{levels: levels}, nil
}

func (f *FederationState) Levels() []ChainLevel { return f.levels }

// adoptedBlock rebuilds the BlockComputedState of a running alpha from the
// facts it reported, so a reused (or statically-walked) level folds into the
// accumulator through the same Join(block) path as a freshly resolved one —
// no separate glue per source.
func adoptedBlock(st *protocol.StateInfo) alphasfile.BlockComputedState {
	return alphasfile.AdoptBlock(st.Services, st.Toolchain, st.SysEnv, st.Dotenv, st.Env, st.CfgHash)
}

// startConfig bundles the spawn/configure knobs reconcileAlpha needs so the
// per-level effect call doesn't thread six loose arguments.
type startConfig struct {
	alphaBin string
	alphaLog string
	timeout  time.Duration
	failfast bool
	verbose  bool
	agent    bool
	summary  bool
	// stdout is where the structured start summary VIEW is written (stderr
	// carries the log-style bringup chatter). The MCP server swaps in a
	// buffer here to capture it as a tool result.
	stdout io.Writer
}

// canReuse is the pure multi-agent-invariant decision: a running level may be
// adopted as-is ONLY when it is a parent (not the leaf) whose live config hash
// still matches. The leaf is never reusable (the user asked to (re)start it
// here); a drifted parent isn't either; a level with no running alpha isn't.
func canReuse(isLeaf bool, old *protocol.StateInfo, cfgHash string) bool {
	return !isLeaf && old != nil && old.CfgHash == cfgHash
}

// resolveLevel decides whether a chain level can reuse its running alpha or
// must (re)compute. The leaf is ALWAYS recomputed (the user ran `zordon start`
// here); a parent is reused as long as it is running and its config hasn't
// drifted — so a leaf start never restarts a healthy shared parent other
// agents depend on (see canReuse). A parent restarts only on drift, never
// "by the way".
//
//	reused=true  → adopt the running alpha as-is (af is nil; old carries its state)
//	reused=false → af is freshly resolved; old (if non-nil) is a running alpha
//	               the caller must stop before spawning the replacement.
func resolveLevel(ctx context.Context, lvl ChainLevel, cfgHash string, parentCtx *alphasfile.ParentContext, testCfg alphasfile.TestConfig) (af *alphasfile.Alphasfile, old *protocol.StateInfo, reused bool, err error) {
	if resp, e := control.Roundtrip(ctx, lvl.inv.SocketPath(), &protocol.Request{Op: protocol.OpState}); e == nil && resp != nil && resp.State != nil {
		old = resp.State
	}
	if canReuse(lvl.IsLeaf(), old, cfgHash) {
		return nil, old, true, nil
	}
	// świeży: parse → Plan (kolejność; cykl pada TU) → Compute (eval + efekty).
	// Używa już-wczytanych bajtów (lvl.bytes), więc nie czyta pliku ponownie.
	man, err := alphasfile.NewManifestState(lvl.afPath, lvl.bytes, lvl.inv)
	if err != nil {
		return nil, old, false, err
	}
	plan, err := man.Plan(parentCtx, cfgHash, testCfg)
	if err != nil {
		return nil, old, false, err
	}
	af, err = plan.Compute()
	if err != nil {
		return nil, old, false, err
	}
	return af, old, false, nil
}

// reconcileAlpha brings a freshly resolved level's alpha into being: stop a
// drifted/old alpha if one is still running, then spawn + configure the
// replacement and block until it has accepted the config (EventDone) or
// failed. Resolution already happened (resolveLevel) — so a config that
// won't even parse never tears down the old, healthy alpha.
func reconcileAlpha(ctx context.Context, lvl ChainLevel, af *alphasfile.Alphasfile, old *protocol.StateInfo, parentDotenv []string, parentEnv map[string]string, cfg startConfig, log *zlog.Logger) error {
	inv := lvl.inv
	sock := inv.SocketPath()

	if old != nil {
		reason := "restart requested"
		if !lvl.IsLeaf() {
			reason = "drift detected (config changed since alpha started)"
		}
		log.Info("zordon", "%s [%s]: %s, restarting alpha pid=%d", lvl.afPath, inv.FsHash, reason, old.PID)
		if _, e := control.Roundtrip(ctx, sock, &protocol.Request{Op: protocol.OpShutdown}); e != nil {
			log.Error("zordon", "shutdown %s: %v", lvl.afPath, e)
		}
		if err := waitSocketGone(ctx, sock, cfg.timeout); err != nil {
			return fmt.Errorf("%s: waiting for old alpha to exit: %w", lvl.afPath, err)
		}
	}

	levelLog := inv.AlphaLogPath()
	if lvl.IsLeaf() {
		levelLog = zfs.NewResolver(inv.Dir, inv.FsHash).AlphaLogFile(cfg.alphaLog)
	}
	if err := zfs.EnsureSharedDir(filepath.Dir(levelLog)); err != nil {
		return fmt.Errorf("mkdir state dir: %w", err)
	}
	// The socket lives in inv.TmpDir ($TMPDIR/zordon-<FsHash>); alpha can't
	// bind into a missing directory.
	if err := zfs.EnsureSharedDir(inv.TmpDir); err != nil {
		return fmt.Errorf("mkdir tmp dir: %w", err)
	}

	ctxLevel, cancel := context.WithTimeout(ctx, cfg.timeout)
	defer cancel()
	if err := spawnAlpha(cfg.alphaBin, levelLog, sock, cfg.timeout, cfg.verbose, log, inv.Env); err != nil {
		return fmt.Errorf("%s: %w", lvl.afPath, err)
	}
	if err := control.WaitListening(ctxLevel, sock); err != nil {
		return fmt.Errorf("%s: waiting for alpha socket: %w", lvl.afPath, err)
	}
	if err := pushConfigure(ctxLevel, log, cfg.stdout, sock, lvl.afPath, inv.FsHash, af.CfgHash, parentDotenv, parentEnv, af, cfg.failfast, cfg.agent, cfg.summary || cfg.verbose); err != nil {
		return fmt.Errorf("%s: %w", lvl.afPath, err)
	}
	return nil
}
