package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/piotrkowalczuk/zordon/internal/alphasfile"
	"github.com/piotrkowalczuk/zordon/internal/protocol"
	"github.com/piotrkowalczuk/zordon/internal/zlock"
	"github.com/piotrkowalczuk/zordon/internal/zlog"
)

func TestToolchainStreamWriter(t *testing.T) {
	var logBuf, wireBuf bytes.Buffer
	log := zlog.New(&logBuf, true)
	stream := newSafeEncoder(protocol.NewEncoder(&wireBuf))

	w := newToolchainStreamWriter("toolchain/go", "stderr", stream, log)
	_, _ = w.Write([]byte("downloading 10%\rdownloading 100%\r\npartial "))
	_, _ = w.Write([]byte("line\n\ntrailing"))
	w.Flush()

	events := decodeTestEvents(t, &wireBuf)
	if len(events) != 3 {
		t.Fatalf("got %d events, want 3: %+v", len(events), events)
	}
	wantLines := []string{"downloading 100%", "partial line", "trailing"}
	for i, want := range wantLines {
		if events[i].Kind != protocol.EventLog || events[i].Service != "toolchain/go" || events[i].Stream != "stderr" || events[i].Line != want {
			t.Errorf("event[%d] = %+v, want EventLog toolchain/go stderr %q", i, events[i], want)
		}
	}
	if !strings.Contains(logBuf.String(), "downloading 100%") {
		t.Errorf("alpha log missing streamed line: %s", logBuf.String())
	}
}

func TestDescribeBringupWait_reportsToolchainPhases(t *testing.T) {
	start := time.Now()
	state := &alphaState{shutdownCh: make(chan struct{})}

	goTC := newToolchainCtx("go", "1.26.0", nil, nil)
	goTC.key = "go"
	goTC.setPhase("installing mise")
	goTC.lifecycle.Reach("installing")
	state.addToolchain(goTC)

	pkgTC := newToolchainCtx(alphasfile.ToolchainPkg, "", nil, nil)
	pkgTC.key = "pkg"
	pkgTC.pkgTools = map[string]string{"aqua:ariga/atlas": "1.2.0"}
	pkgTC.setPhase("installing aqua:ariga/atlas@1.2.0")
	pkgTC.lifecycle.Reach("installing")
	state.addToolchain(pkgTC)

	api := &alphasfile.Service{
		Toolchain: "go",
		Runtime:   &alphasfile.RuntimeConfig{Name: "api"},
	}
	ctxs := map[string]*serviceCtx{"api": newServiceCtx("api", "go")}

	got := describeBringupWait([]*alphasfile.Service{api}, ctxs, nil, state, start, start.Add(5*time.Second))
	want := "api (toolchain go@1.26.0: installing mise), toolchain/pkg (installing aqua:ariga/atlas@1.2.0)"
	if got != want {
		t.Fatalf("describeBringupWait = %q, want %q", got, want)
	}

	var largeSvcs []*alphasfile.Service
	largeCtxs := make(map[string]*serviceCtx)
	for i := range 4 {
		name := fmt.Sprintf("svc-%d", i)
		largeSvcs = append(largeSvcs, &alphasfile.Service{
			Toolchain: "go",
			Runtime:   &alphasfile.RuntimeConfig{Name: name},
		})
		largeCtxs[name] = newServiceCtx(name, "go")
	}
	gotLarge := describeBringupWait(largeSvcs, largeCtxs, nil, state, start, start.Add(10*time.Second))
	if !strings.Contains(gotLarge, "4 toolchain go@1.26.0: installing mise") {
		t.Fatalf("large summary missing toolchain phase count: %q", gotLarge)
	}
}

func TestHandleConfigure_streamsToolchainInstallProgress(t *testing.T) {
	home := t.TempDir()
	writeFakeMise(t, home, false)

	state := &alphaState{
		zordonHome: home,
		keychain:   zlock.NewGlobalKeychain(home),
		shutdownCh: make(chan struct{}),
	}
	var logBuf, wireBuf bytes.Buffer
	log := zlog.New(&logBuf, true)

	req := &protocol.Request{
		Op: protocol.OpConfigure,
		Configure: &protocol.ConfigureArgs{
			AlphasfilePath: filepath.Join(home, "Alphasfile"),
			Failfast:       true,
			Alphasfile: &alphasfile.Alphasfile{
				Toolchain: map[string]*alphasfile.ToolchainConfig{
					"go": {Version: "1.26.0"},
				},
			},
		},
	}

	handleConfigure(req, state, bringupConfig{stabilization: 10 * time.Millisecond, shutdownGrace: 100 * time.Millisecond}, protocol.NewEncoder(&wireBuf), log)

	events := decodeTestEvents(t, &wireBuf)
	var sawInstallLine, sawDone bool
	for _, ev := range events {
		if ev.Kind == protocol.EventLog && ev.Service == "toolchain/go" && strings.Contains(ev.Line, "fake-mise: installing go@1.26.0") {
			sawInstallLine = true
		}
		if ev.Kind == protocol.EventDone {
			sawDone = true
		}
	}
	if !sawInstallLine {
		t.Fatalf("expected streamed EventLog from toolchain/go, got events: %+v", events)
	}
	if !sawDone {
		t.Fatalf("expected EventDone on successful configure, got events: %+v", events)
	}
}

func TestHandleConfigure_toolchainFailureReportsCulpritWithoutServiceFallout(t *testing.T) {
	home := t.TempDir()
	writeFakeMise(t, home, true)

	state := &alphaState{
		zordonHome: home,
		keychain:   zlock.NewGlobalKeychain(home),
		shutdownCh: make(chan struct{}),
	}
	var logBuf, wireBuf bytes.Buffer
	log := zlog.New(&logBuf, true)

	req := &protocol.Request{
		Op: protocol.OpConfigure,
		Configure: &protocol.ConfigureArgs{
			AlphasfilePath: filepath.Join(home, "Alphasfile"),
			Failfast:       true,
			Alphasfile: &alphasfile.Alphasfile{
				Toolchain: map[string]*alphasfile.ToolchainConfig{
					"go": {Version: "1.26.0"},
				},
				Services: []*alphasfile.Service{
					{
						Toolchain: "go",
						Runtime: &alphasfile.RuntimeConfig{
							Name:    "api",
							Command: []string{"/bin/sh", "-c", "sleep 10"},
							Log:     &alphasfile.LogConfig{TTY: new(false)},
						},
					},
				},
			},
		},
	}

	handleConfigure(req, state, bringupConfig{stabilization: 10 * time.Millisecond, shutdownGrace: 100 * time.Millisecond}, protocol.NewEncoder(&wireBuf), log)

	events := decodeTestEvents(t, &wireBuf)
	var (
		sawTailLine  bool
		failServices []string
		sawFailfast  bool
	)
	for _, ev := range events {
		switch ev.Kind {
		case protocol.EventLog:
			if ev.Service == "toolchain/go" && strings.Contains(ev.Line, "FATAL: upstream tarball corrupted") {
				sawTailLine = true
			}
		case protocol.EventServiceFail:
			failServices = append(failServices, ev.Service+":"+ev.Error)
		case protocol.EventError:
			if strings.Contains(ev.Error, "failfast: toolchain/go:") {
				sawFailfast = true
			}
		}
	}

	if !sawTailLine {
		t.Fatalf("missing streamed failure log line for toolchain/go; events=%+v", events)
	}
	if len(failServices) != 1 || !strings.HasPrefix(failServices[0], "toolchain/go:") {
		t.Fatalf("expected sole EventServiceFail on toolchain/go (no api fallout), got %v", failServices)
	}
	if !sawFailfast {
		t.Fatalf("expected EventError with failfast: toolchain/go, got events=%+v", events)
	}
}

func TestHandleConfigure_honorsMiseVersionOverride(t *testing.T) {
	home := t.TempDir()
	customVer := "2026.10.4"
	writeFakeMiseAtVersion(t, home, customVer, false)

	state := &alphaState{
		zordonHome: home,
		keychain:   zlock.NewGlobalKeychain(home),
		shutdownCh: make(chan struct{}),
	}
	var logBuf, wireBuf bytes.Buffer
	log := zlog.New(&logBuf, true)

	req := &protocol.Request{
		Op: protocol.OpConfigure,
		Configure: &protocol.ConfigureArgs{
			AlphasfilePath: filepath.Join(home, "Alphasfile"),
			Failfast:       true,
			MiseVersion:    "v" + customVer,
			Alphasfile: &alphasfile.Alphasfile{
				Toolchain: map[string]*alphasfile.ToolchainConfig{
					"go": {Version: "1.26.0"},
				},
			},
		},
	}

	handleConfigure(req, state, bringupConfig{stabilization: 10 * time.Millisecond, shutdownGrace: 100 * time.Millisecond}, protocol.NewEncoder(&wireBuf), log)

	events := decodeTestEvents(t, &wireBuf)
	var sawDone bool
	for _, ev := range events {
		if ev.Kind == protocol.EventDone {
			sawDone = true
		}
	}
	if !sawDone {
		t.Fatalf("expected EventDone with matching custom --mise.version=%s, got events=%+v", customVer, events)
	}
}

func decodeTestEvents(t *testing.T, r io.Reader) []protocol.Event {
	t.Helper()
	dec := protocol.NewDecoder(r)
	var out []protocol.Event
	for {
		var ev protocol.Event
		err := dec.Read(&ev)
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatalf("decode event: %v", err)
		}
		out = append(out, ev)
	}
}

func writeFakeMise(t *testing.T, home string, failInstall bool) {
	t.Helper()
	writeFakeMiseAtVersion(t, home, "2026.11.6", failInstall)
}

func writeFakeMiseAtVersion(t *testing.T, home, version string, failInstall bool) {
	t.Helper()
	binDir := filepath.Join(home, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	var body string
	if failInstall {
		body = fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then echo %q; exit 0; fi\nshift # --no-config\nif [ \"$1\" = \"install\" ]; then\n  echo \"FATAL: upstream tarball corrupted\" >&2\n  exit 42\nfi\nexit 1\n", version)
	} else {
		body = fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then echo %q; exit 0; fi\nshift # --no-config\ncase \"$1\" in\n  install)\n    echo \"fake-mise: installing $2\" >&2\n    exit 0\n    ;;\n  env)\n    printf '{\"PATH\":\"/usr/bin:/bin\"}\\n'\n    exit 0\n    ;;\nesac\nexit 1\n", version)
	}
	if err := os.WriteFile(filepath.Join(binDir, "mise"), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}
