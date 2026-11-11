package tools

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/piotrkowalczuk/zordon/internal/zfs"
	"github.com/piotrkowalczuk/zordon/internal/zhttp"
	"github.com/piotrkowalczuk/zordon/internal/zlock"
)

func TestMiseVersion_Set(t *testing.T) {
	cases := map[string]struct {
		input   string
		want    string
		wantErr bool
	}{
		"empty_defaults": {
			input: "",
			want:  DefaultMiseVersion,
		},
		"calver": {
			input: "2026.10.4",
			want:  "2026.10.4",
		},
		"v_prefix_stripped": {
			input: "v2026.11.6",
			want:  "2026.11.6",
		},
		"prerelease_allowed": {
			input: "2026.12.0-rc.1",
			want:  "2026.12.0-rc.1",
		},
		"two_segments_rejected": {
			input:   "2026.11",
			wantErr: true,
		},
		"path_traversal_rejected": {
			input:   "2026.11.6/../evil",
			wantErr: true,
		},
		"non_numeric_rejected": {
			input:   "latest",
			wantErr: true,
		},
	}

	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			var v MiseVersion
			err := v.Set(c.input)
			if c.wantErr {
				if err == nil {
					t.Fatalf("Set(%q) = nil, want error", c.input)
				}
				return
			}
			if err != nil {
				t.Fatalf("Set(%q): %v", c.input, err)
			}
			if got := v.Version(); got != c.want {
				t.Errorf("Version() = %q, want %q", got, c.want)
			}
		})
	}
}

func TestParseMiseVersionOutput(t *testing.T) {
	cases := map[string]struct {
		raw  string
		want string
	}{
		"calver_with_target": {
			raw:  "2026.11.6 macos-arm64 (2026-11-08)\n",
			want: "2026.11.6",
		},
		"prefixed_with_mise": {
			raw:  "mise 2026.10.1 linux-x64\n",
			want: "2026.10.1",
		},
		"v_prefix": {
			raw:  "v2026.11.6\n",
			want: "2026.11.6",
		},
		"invalid_text": {
			raw:  "error: unknown flag --version\n",
			want: "",
		},
	}

	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			if got := parseMiseVersionOutput(c.raw); got != c.want {
				t.Errorf("parseMiseVersionOutput(%q) = %q, want %q", c.raw, got, c.want)
			}
		})
	}
}

func TestParseShasums256(t *testing.T) {
	sum := strings.Repeat("ab", 32)
	manifest := strings.Join([]string{
		strings.Repeat("00", 32) + "  ./mise-v2026.10.4-linux-x64",
		sum + "  ./mise-v2026.10.4-macos-arm64.tar.gz",
		strings.Repeat("ff", 32) + " *mise-v2026.10.4-linux-arm64.tar.gz",
	}, "\n")

	got, err := parseShasums256(manifest, "mise-v2026.10.4-macos-arm64.tar.gz")
	if err != nil || got != sum {
		t.Fatalf("parseShasums256(./ entry) = (%q, %v), want (%q, nil)", got, err, sum)
	}
	gotStar, err := parseShasums256(manifest, "mise-v2026.10.4-linux-arm64.tar.gz")
	if err != nil || gotStar != strings.Repeat("ff", 32) {
		t.Fatalf("parseShasums256(* entry) = (%q, %v)", gotStar, err)
	}
	if _, err := parseShasums256(manifest, "mise-v2026.10.4-missing.tar.gz"); err == nil {
		t.Fatal("expected error for missing asset")
	}
}

func TestEnsureMise_reusesMatchingVersionBinary(t *testing.T) {
	home := t.TempDir()
	wantBin := writeFakeMiseVersionBin(t, filepath.Join(home, "bin", "mise"), DefaultMiseVersion)

	var logBuf bytes.Buffer
	gotBin, err := EnsureMise(t.Context(), zlock.NewGlobalKeychain(home), home, "", &logBuf)
	if err != nil {
		t.Fatalf("EnsureMise: %v", err)
	}
	if gotBin != wantBin {
		t.Fatalf("EnsureMise bin = %q, want %q", gotBin, wantBin)
	}
	if logBuf.Len() != 0 {
		t.Fatalf("expected silent reuse of matching binary, got log: %q", logBuf.String())
	}
	if !zfs.Exists(miseStampPath(wantBin)) {
		t.Fatalf("expected version stamp at %s", miseStampPath(wantBin))
	}
}

func TestDefaultMiseAssets(t *testing.T) {
	cases := map[string]struct {
		platform   misePlatform
		wantTarget string
	}{
		"darwin_arm64": {
			platform:   misePlatform{goos: "darwin", goarch: "arm64"},
			wantTarget: "macos-arm64",
		},
		"darwin_amd64": {
			platform:   misePlatform{goos: "darwin", goarch: "amd64"},
			wantTarget: "macos-x64",
		},
		"linux_amd64_gnu": {
			platform:   misePlatform{goos: "linux", goarch: "amd64", musl: false},
			wantTarget: "linux-x64",
		},
		"linux_amd64_musl": {
			platform:   misePlatform{goos: "linux", goarch: "amd64", musl: true},
			wantTarget: "linux-x64-musl",
		},
		"linux_arm64_gnu": {
			platform:   misePlatform{goos: "linux", goarch: "arm64", musl: false},
			wantTarget: "linux-arm64",
		},
		"linux_arm64_musl": {
			platform:   misePlatform{goos: "linux", goarch: "arm64", musl: true},
			wantTarget: "linux-arm64-musl",
		},
		"linux_arm_gnu": {
			platform:   misePlatform{goos: "linux", goarch: "arm", musl: false},
			wantTarget: "linux-armv7",
		},
		"linux_arm_musl": {
			platform:   misePlatform{goos: "linux", goarch: "arm", musl: true},
			wantTarget: "linux-armv7-musl",
		},
	}

	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			if got := misePlatformTargets[c.platform]; got != c.wantTarget {
				t.Errorf("target = %q, want %q", got, c.wantTarget)
			}
			sum, ok := defaultMiseSHA256s[c.platform]
			if !ok {
				t.Fatalf("missing default sha256 for %+v", c.platform)
			}
			if len(sum) != 64 {
				t.Errorf("sha256 length = %d, want 64", len(sum))
			}
			if _, err := hex.DecodeString(sum); err != nil {
				t.Errorf("sha256 invalid hex: %v", err)
			}
		})
	}
}

func TestBootstrapMise_downloadsAndVerifiesArchive(t *testing.T) {
	wantPayload := []byte("#!/bin/sh\necho " + DefaultMiseVersion + "\n")
	archive, sum := makeTestMiseTarGz(t, "mise/bin/mise", wantPayload)
	wantPath := "/mise-v" + DefaultMiseVersion + "-macos-arm64.tar.gz"

	primaryBroken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "upstream down", http.StatusBadGateway)
	}))
	defer primaryBroken.Close()

	mirrorOK := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != wantPath {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(archive)
	}))
	defer mirrorOK.Close()

	home := t.TempDir()
	bin := filepath.Join(home, "bin", "mise")

	cfg := miseBootstrapConfig{
		version:  DefaultMiseVersion,
		baseURLs: []string{primaryBroken.URL, mirrorOK.URL},
		goos:     "darwin",
		goarch:   "arm64",
		targets:  misePlatformTargets,
		pinnedSHA256s: map[misePlatform]string{
			{goos: "darwin", goarch: "arm64"}: sum,
		},
		client: zhttp.NewClient(zhttp.Options{Timeout: 5 * time.Second}),
		lookPath: func(name string) (string, error) {
			return "", fmt.Errorf("unexpected lookPath(%q) on release success", name)
		},
	}

	var logBuf bytes.Buffer
	gotBin, err := ensureMiseWithConfig(t.Context(), zlock.NewGlobalKeychain(home), cfg, home, &logBuf)
	if err != nil {
		t.Fatalf("ensureMiseWithConfig: %v", err)
	}
	if gotBin != bin || !zfs.IsExecutableFile(bin) {
		t.Fatalf("gotBin = %q, executable = %v", gotBin, zfs.IsExecutableFile(bin))
	}
	gotPayload, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotPayload, wantPayload) {
		t.Fatalf("binary payload = %q, want %q", string(gotPayload), string(wantPayload))
	}
	if !strings.Contains(logBuf.String(), "bootstrapped mise v"+DefaultMiseVersion) {
		t.Fatalf("log missing bootstrap confirmation: %q", logBuf.String())
	}
}

func TestBootstrapMise_customVersionFetchesShasumsAndReplacesBinary(t *testing.T) {
	home := t.TempDir()
	bin := writeFakeMiseVersionBin(t, filepath.Join(home, "bin", "mise"), DefaultMiseVersion)

	customVer := "2026.10.4"
	assetName := "mise-v" + customVer + "-macos-arm64.tar.gz"
	wantPayload := []byte("#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then echo \"" + customVer + "\"; exit 0; fi\n")
	archive, sum := makeTestMiseTarGz(t, "mise/bin/mise", wantPayload)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/SHASUMS256.txt":
			_, _ = fmt.Fprintf(w, "%s  ./%s\n", sum, assetName)
		case "/" + assetName:
			_, _ = w.Write(archive)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	cfg := miseBootstrapConfig{
		version:  customVer,
		baseURLs: []string{srv.URL},
		goos:     "darwin",
		goarch:   "arm64",
		targets:  misePlatformTargets,
		client:   zhttp.NewClient(zhttp.Options{Timeout: 5 * time.Second}),
		lookPath: func(name string) (string, error) {
			return "", fmt.Errorf("unexpected lookPath(%q) on custom release success", name)
		},
	}

	var logBuf bytes.Buffer
	gotBin, err := ensureMiseWithConfig(t.Context(), zlock.NewGlobalKeychain(home), cfg, home, &logBuf)
	if err != nil {
		t.Fatalf("ensureMiseWithConfig custom version: %v", err)
	}
	if gotBin != bin {
		t.Fatalf("gotBin = %q, want %q", gotBin, bin)
	}
	ver, ok := detectInstalledMiseVersion(t.Context(), bin)
	if !ok || ver != customVer {
		t.Fatalf("installed version = (%q, %v), want (%q, true)", ver, ok, customVer)
	}
	if !strings.Contains(logBuf.String(), "is v"+DefaultMiseVersion+" (want v"+customVer+"); updating") {
		t.Fatalf("log missing update notice: %q", logBuf.String())
	}
}

func TestBootstrapMise_mismatchedBinaryNeverUsedWithoutUpgrade(t *testing.T) {
	home := t.TempDir()
	_ = writeFakeMiseVersionBin(t, filepath.Join(home, "bin", "mise"), "2026.1.0")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "offline", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	cfg := miseBootstrapConfig{
		version:  DefaultMiseVersion,
		baseURLs: []string{srv.URL},
		goos:     "darwin",
		goarch:   "arm64",
		targets:  misePlatformTargets,
		pinnedSHA256s: map[misePlatform]string{
			{goos: "darwin", goarch: "arm64"}: strings.Repeat("a", 64),
		},
		client: zhttp.NewClient(zhttp.Options{Timeout: 5 * time.Second}),
		lookPath: func(string) (string, error) {
			return "", errors.New("not found")
		},
	}

	var logBuf bytes.Buffer
	if _, err := ensureMiseWithConfig(t.Context(), zlock.NewGlobalKeychain(home), cfg, home, &logBuf); err == nil {
		t.Fatal("expected error when outdated binary cannot be upgraded via release or cargo")
	}
}

func TestBootstrapMise_checksumMismatchFallsBackToCargo(t *testing.T) {
	archive, _ := makeTestMiseTarGz(t, "mise/bin/mise", []byte("tampered"))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(archive)
	}))
	defer srv.Close()

	home := t.TempDir()
	bin := filepath.Join(home, "bin", "mise")
	fakeCargo, argsFile := writeFakeCargo(t, DefaultMiseVersion, true)

	cfg := miseBootstrapConfig{
		version:  DefaultMiseVersion,
		baseURLs: []string{srv.URL},
		goos:     "linux",
		goarch:   "amd64",
		targets:  misePlatformTargets,
		pinnedSHA256s: map[misePlatform]string{
			{goos: "linux", goarch: "amd64"}: strings.Repeat("0", 64),
		},
		client: zhttp.NewClient(zhttp.Options{Timeout: 5 * time.Second}),
		lookPath: func(name string) (string, error) {
			if name == "cargo" {
				return fakeCargo, nil
			}
			return "", errors.New("not found")
		},
	}

	var logBuf bytes.Buffer
	gotBin, err := ensureMiseWithConfig(t.Context(), zlock.NewGlobalKeychain(home), cfg, home, &logBuf)
	if err != nil {
		t.Fatalf("ensureMiseWithConfig cargo fallback: %v", err)
	}
	if gotBin != bin || !zfs.IsExecutableFile(bin) {
		t.Fatalf("cargo fallback did not produce executable %s (got %q)", bin, gotBin)
	}
	gotArgs, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	wantArgs := fmt.Sprintf("install --root %s --locked --version %s mise", home, DefaultMiseVersion)
	if strings.TrimSpace(string(gotArgs)) != wantArgs {
		t.Fatalf("cargo args = %q, want %q", strings.TrimSpace(string(gotArgs)), wantArgs)
	}
	if !strings.Contains(logBuf.String(), "sha256 mismatch") || !strings.Contains(logBuf.String(), "falling back to cargo install") {
		t.Fatalf("log missing checksum mismatch fallback notice: %q", logBuf.String())
	}
}

func TestBootstrapMise_unsupportedPlatformFallsBackToCargo(t *testing.T) {
	home := t.TempDir()
	fakeCargo, _ := writeFakeCargo(t, DefaultMiseVersion, true)

	cfg := miseBootstrapConfig{
		version: DefaultMiseVersion,
		goos:    "freebsd",
		goarch:  "riscv64",
		targets: misePlatformTargets,
		client:  zhttp.NewClient(zhttp.Options{Timeout: 5 * time.Second}),
		lookPath: func(name string) (string, error) {
			if name == "cargo" {
				return fakeCargo, nil
			}
			return "", errors.New("not found")
		},
	}

	var logBuf bytes.Buffer
	if _, err := ensureMiseWithConfig(t.Context(), zlock.NewGlobalKeychain(home), cfg, home, &logBuf); err != nil {
		t.Fatalf("ensureMiseWithConfig unsupported platform: %v", err)
	}
	if !strings.Contains(logBuf.String(), "no prebuilt mise v"+DefaultMiseVersion+" release binary for freebsd/riscv64; falling back to cargo install") {
		t.Fatalf("log missing unsupported platform fallback: %q", logBuf.String())
	}
}

func TestBootstrapMise_downloadAndCargoBothFailReportsBoth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	home := t.TempDir()
	fakeCargoFail, _ := writeFakeCargo(t, DefaultMiseVersion, false)

	cfg := miseBootstrapConfig{
		version:  DefaultMiseVersion,
		baseURLs: []string{srv.URL},
		goos:     "darwin",
		goarch:   "arm64",
		targets:  misePlatformTargets,
		pinnedSHA256s: map[misePlatform]string{
			{goos: "darwin", goarch: "arm64"}: strings.Repeat("a", 64),
		},
		client: zhttp.NewClient(zhttp.Options{Timeout: 5 * time.Second}),
		lookPath: func(name string) (string, error) {
			if name == "cargo" {
				return fakeCargoFail, nil
			}
			return "", errors.New("not found")
		},
	}

	var logBuf bytes.Buffer
	_, err := ensureMiseWithConfig(t.Context(), zlock.NewGlobalKeychain(home), cfg, home, &logBuf)
	if err == nil {
		t.Fatal("expected error when release download and cargo both fail")
	}
	msg := err.Error()
	if !strings.Contains(msg, "503") || !strings.Contains(msg, "cargo fallback failed") || !strings.Contains(msg, "cmake was not found on PATH") {
		t.Fatalf("error missing combined cause or cmake hint: %q", msg)
	}
}

func TestEnsureToolchain_skipsWhenAlreadyInstalled(t *testing.T) {
	dataDir := t.TempDir()
	installDir := filepath.Join(dataDir, "installs", "node", "22.14.0")
	if err := os.MkdirAll(installDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if !IsInstalled(dataDir, "nodejs", "22.14.0") {
		t.Fatal("expected IsInstalled(nodejs@22.14.0) to map nodejs -> node and return true")
	}
	if err := EnsureToolchain("/nonexistent/mise", dataDir, "nodejs", "22.14.0", nil); err != nil {
		t.Fatalf("EnsureToolchain warm install invoked nonexistent mise binary: %v", err)
	}
}

func writeFakeMiseVersionBin(t *testing.T, path, version string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	script := fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then echo \"%s test-arch\"; exit 0; fi\necho \"%s\"\n", version, version)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func makeTestMiseTarGz(t *testing.T, entryName string, payload []byte) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	gzw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gzw)
	hdr := &tar.Header{
		Name:     entryName,
		Mode:     0o755,
		Size:     int64(len(payload)),
		Typeflag: tar.TypeReg,
	}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzw.Close(); err != nil {
		t.Fatal(err)
	}
	raw := buf.Bytes()
	sum := sha256.Sum256(raw)
	return raw, hex.EncodeToString(sum[:])
}

func writeFakeCargo(t *testing.T, version string, succeed bool) (string, string) {
	t.Helper()
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "cargo.args")
	scriptPath := filepath.Join(dir, "cargo")
	var script string
	if succeed {
		script = fmt.Sprintf("#!/bin/sh\nset -eu\nprintf '%%s\\n' \"$*\" > %q\nroot=\"\"\nwhile [ $# -gt 0 ]; do\n  if [ \"$1\" = \"--root\" ]; then root=\"$2\"; shift 2; else shift; fi\ndone\nmkdir -p \"$root/bin\"\nprintf '#!/bin/sh\\nif [ \"$1\" = \"--version\" ]; then echo \"%s\"; exit 0; fi\\necho cargo-mise\\n' > \"$root/bin/mise\"\nchmod 0755 \"$root/bin/mise\"\n", argsFile, version)
	} else {
		script = fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$*\" > %q\necho 'failed to run custom build command for libz-ng-sys' >&2\nexit 101\n", argsFile)
	}
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return scriptPath, argsFile
}
