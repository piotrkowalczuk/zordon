package tools

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/piotrkowalczuk/zordon/internal/buildquota"
	"github.com/piotrkowalczuk/zordon/internal/daemon"
	"github.com/piotrkowalczuk/zordon/internal/zenv"
	"github.com/piotrkowalczuk/zordon/internal/zfs"
	"github.com/piotrkowalczuk/zordon/internal/zhttp"
	"github.com/piotrkowalczuk/zordon/internal/zlock"
)

// DefaultMiseVersion is the mise release version verified against this build
// of zordon and used whenever --mise.version / ZORDON_MISE_VERSION is unset.
const DefaultMiseVersion = "2026.11.6"

// MiseVersion is a validated mise release version (e.g. "2026.11.6").
// Its zero value represents DefaultMiseVersion.
type MiseVersion string

func (v *MiseVersion) String() string {
	if v == nil || *v == "" {
		return DefaultMiseVersion
	}
	return string(*v)
}

func (v *MiseVersion) Set(s string) error {
	s = strings.TrimSpace(s)
	if s == "" {
		*v = DefaultMiseVersion
		return nil
	}
	norm, err := normalizeMiseVersion(s)
	if err != nil {
		return err
	}
	*v = MiseVersion(norm)
	return nil
}

// Version returns the effective version string (DefaultMiseVersion for zero).
func (v MiseVersion) Version() string {
	if v == "" {
		return DefaultMiseVersion
	}
	return string(v)
}

// EnsureMise ensures <zordonHome>/bin/mise exists at the exact requested
// version (DefaultMiseVersion when reqVersion is zero):
//  1. If <zordonHome>/bin/mise already matches the requested version, reuse it.
//  2. Otherwise download and SHA-256-verify the release archive for the
//     platform (compiled-in checksums for DefaultMiseVersion, release
//     SHASUMS256.txt for explicit overrides) and atomically replace
//     <zordonHome>/bin/mise.
//  3. Only when no prebuilt release exists or download fails, fall back to
//     compiling that exact version via `cargo install --locked --version <ver> mise`.
func EnsureMise(ctx context.Context, kc zlock.GlobalKeychain, limiter *buildquota.Limiter, setPhase func(string), zordonHome string, reqVersion MiseVersion, logOut io.Writer) (string, error) {
	if ctx == nil {
		panic("tools: EnsureMise requires non-nil context")
	}
	if limiter == nil {
		panic("tools: EnsureMise requires non-nil limiter")
	}
	ver := reqVersion.Version()
	if _, err := normalizeMiseVersion(ver); err != nil {
		panic("tools: EnsureMise called with unvalidated MiseVersion: " + err.Error())
	}
	if logOut == nil {
		logOut = io.Discard
	}
	cfg := defaultMiseBootstrapConfig(ver)
	cfg.limiter = limiter
	cfg.setPhase = setPhase
	return ensureMiseWithConfig(ctx, kc, cfg, zordonHome, logOut)
}

const (
	maxMiseArchiveBytes = 150 << 20
	maxMiseBinaryBytes  = 300 << 20
	maxMiseShasumsBytes = 64 << 10
	progressStepBytes   = 10 << 20
)

type misePlatform struct {
	goos   string
	goarch string
	musl   bool
}

type miseBootstrapConfig struct {
	version       string
	baseURLs      []string
	goos          string
	goarch        string
	musl          bool
	targets       map[misePlatform]string
	pinnedSHA256s map[misePlatform]string
	client        *zhttp.Client
	lookPath      func(string) (string, error)
	limiter       *buildquota.Limiter
	setPhase      func(string)
}

var misePlatformTargets = map[misePlatform]string{
	{goos: "darwin", goarch: "arm64"}:             "macos-arm64",
	{goos: "darwin", goarch: "amd64"}:             "macos-x64",
	{goos: "linux", goarch: "amd64", musl: false}: "linux-x64",
	{goos: "linux", goarch: "amd64", musl: true}:  "linux-x64-musl",
	{goos: "linux", goarch: "arm64", musl: false}: "linux-arm64",
	{goos: "linux", goarch: "arm64", musl: true}:  "linux-arm64-musl",
	{goos: "linux", goarch: "arm", musl: false}:   "linux-armv7",
	{goos: "linux", goarch: "arm", musl: true}:    "linux-armv7-musl",
}

var defaultMiseSHA256s = map[misePlatform]string{
	{goos: "darwin", goarch: "arm64"}:             "74cf55e92f6c6b1f166781c36db35db9420a4cbf40d62d85c1b9d5dc76e0ba72",
	{goos: "darwin", goarch: "amd64"}:             "052bf6eb24e5e813d42f6959f11340dba1c8e021b5cc7edda497c8d0c12d9948",
	{goos: "linux", goarch: "amd64", musl: false}: "d6ebb6f5601bdf5a8d1d56028b24fe575f45075b989941ccc920f95161c5fa28",
	{goos: "linux", goarch: "amd64", musl: true}:  "2cc1157b9d75a1c6a63aa7231a047c33b43f273fd2bc545ab51ae9db4f74993a",
	{goos: "linux", goarch: "arm64", musl: false}: "c5c90bea46cae89fd02d09a034e5e2d171abbbb313dab9baa833ee65a8cd996c",
	{goos: "linux", goarch: "arm64", musl: true}:  "982a195d32427364beb878e1933b76aa5b1d1ccf82c3aca8cfcc2675666546a2",
	{goos: "linux", goarch: "arm", musl: false}:   "c7759133e947b4f045caa95049448b8339f5caa91716ef62bfb5bf52284367f4",
	{goos: "linux", goarch: "arm", musl: true}:    "b64b28405c6bf8cf9e6ef6055916a409d9e9ae421a62ade55369b7658e0174a6",
}

func defaultMiseBootstrapConfig(version string) miseBootstrapConfig {
	var pinned map[misePlatform]string
	if version == DefaultMiseVersion {
		pinned = defaultMiseSHA256s
	}
	return miseBootstrapConfig{
		version: version,
		baseURLs: []string{
			"https://github.com/jdx/mise/releases/download/v" + version,
			"https://mise.jdx.dev/v" + version,
		},
		goos:          runtime.GOOS,
		goarch:        runtime.GOARCH,
		musl:          detectMuslLinux(runtime.GOOS, runtime.GOARCH),
		targets:       misePlatformTargets,
		pinnedSHA256s: pinned,
		client:        zhttp.NewClient(zhttp.Options{Timeout: 5 * time.Minute}),
		lookPath:      exec.LookPath,
	}
}

func normalizeMiseVersion(raw string) (string, error) {
	v := strings.TrimPrefix(strings.TrimSpace(raw), "v")
	if v == "" {
		return "", fmt.Errorf("tools: invalid mise version %q: empty version", raw)
	}
	core, pre, hasPre := strings.Cut(v, "-")
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return "", fmt.Errorf("tools: invalid mise version %q: expected X.Y.Z (e.g. %s)", raw, DefaultMiseVersion)
	}
	for _, p := range parts {
		if p == "" {
			return "", fmt.Errorf("tools: invalid mise version %q: empty numeric segment", raw)
		}
		for i := range len(p) {
			if p[i] < '0' || p[i] > '9' {
				return "", fmt.Errorf("tools: invalid mise version %q: non-numeric segment %q", raw, p)
			}
		}
	}
	if hasPre {
		if pre == "" || strings.HasPrefix(pre, ".") || strings.HasSuffix(pre, ".") || strings.Contains(pre, "..") {
			return "", fmt.Errorf("tools: invalid mise version %q: malformed prerelease suffix", raw)
		}
		for i := range len(pre) {
			c := pre[i]
			ok := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '.' || c == '-'
			if !ok {
				return "", fmt.Errorf("tools: invalid mise version %q: invalid character %q in prerelease", raw, c)
			}
		}
	}
	return v, nil
}

func ensureMiseWithConfig(ctx context.Context, kc zlock.GlobalKeychain, cfg miseBootstrapConfig, zordonHome string, logOut io.Writer) (string, error) {
	bin := filepath.Join(zordonHome, "bin", "mise")
	if ver, ok := detectInstalledMiseVersion(ctx, bin); ok && ver == cfg.version {
		return bin, nil
	}
	if err := zfs.EnsureDir(filepath.Join(zordonHome, "bin")); err != nil {
		return "", err
	}
	release, err := kc.LockMise(ctx)
	if err != nil {
		return "", err
	}
	defer release()

	existingVer, hasExisting := detectInstalledMiseVersion(ctx, bin)
	if hasExisting && existingVer == cfg.version {
		return bin, nil
	}
	if err := bootstrapMise(ctx, cfg, zordonHome, bin, existingVer, hasExisting, logOut); err != nil {
		return "", err
	}
	writeMiseStamp(bin, cfg.version)
	return bin, nil
}

func detectMuslLinux(goos, goarch string) bool {
	if goos != "linux" {
		return false
	}
	switch goarch {
	case "amd64":
		return zfs.Exists("/lib/ld-musl-x86_64.so.1")
	case "arm64":
		return zfs.Exists("/lib/ld-musl-aarch64.so.1")
	case "arm":
		return zfs.Exists("/lib/ld-musl-armhf.so.1") || zfs.Exists("/lib/ld-musl-arm.so.1")
	default:
		return false
	}
}

func bootstrapMise(ctx context.Context, cfg miseBootstrapConfig, zordonHome, bin, existingVer string, hasExisting bool, logOut io.Writer) error {
	if hasExisting {
		_, _ = fmt.Fprintf(logOut, "mise at %s is v%s (want v%s); updating\n", bin, existingVer, cfg.version)
	}

	platform := misePlatform{goos: cfg.goos, goarch: cfg.goarch, musl: cfg.musl}
	target, hasTarget := cfg.targets[platform]
	if !hasTarget {
		_, _ = fmt.Fprintf(logOut, "no prebuilt mise v%s release binary for %s/%s; falling back to cargo install\n", cfg.version, cfg.goos, cfg.goarch)
		return cargoInstallMise(ctx, cfg, zordonHome, bin, logOut)
	}

	assetName := fmt.Sprintf("mise-v%s-%s.tar.gz", cfg.version, target)
	pinnedSum := ""
	if cfg.pinnedSHA256s != nil {
		pinnedSum = cfg.pinnedSHA256s[platform]
	}
	dlErr := downloadAndInstallMise(ctx, cfg, assetName, pinnedSum, bin, logOut)
	if dlErr == nil {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}

	_, _ = fmt.Fprintf(logOut, "prebuilt mise v%s download failed (%v); falling back to cargo install\n", cfg.version, dlErr)
	if fbErr := cargoInstallMise(ctx, cfg, zordonHome, bin, logOut); fbErr != nil {
		return fmt.Errorf("bootstrap mise v%s: release download failed (%w) and cargo fallback failed: %v", cfg.version, dlErr, fbErr)
	}
	return nil
}

func miseStampPath(bin string) string {
	return filepath.Join(filepath.Dir(bin), ".mise-version")
}

func detectInstalledMiseVersion(ctx context.Context, bin string) (string, bool) {
	if !zfs.IsExecutableFile(bin) {
		return "", false
	}
	info, err := zfs.Stat(bin)
	if err != nil {
		return "", false
	}
	if data, err := zfs.Read(miseStampPath(bin)); err == nil {
		fields := strings.Fields(string(data))
		if len(fields) == 3 {
			size, sErr := strconv.ParseInt(fields[1], 10, 64)
			mtime, mErr := strconv.ParseInt(fields[2], 10, 64)
			if sErr == nil && mErr == nil && size == info.Size() && mtime == info.ModTime().UnixNano() {
				return fields[0], true
			}
		}
	}
	ver, ok := probeMiseBinaryVersion(ctx, bin)
	if ok {
		writeMiseStamp(bin, ver)
	}
	return ver, ok
}

func writeMiseStamp(bin, version string) {
	info, err := zfs.Stat(bin)
	if err != nil || version == "" {
		return
	}
	stamp := fmt.Sprintf("%s %d %d\n", version, info.Size(), info.ModTime().UnixNano())
	_ = zfs.AtomicWrite(miseStampPath(bin), []byte(stamp))
}

func probeMiseBinaryVersion(ctx context.Context, bin string) (string, bool) {
	if !zfs.IsExecutableFile(bin) {
		return "", false
	}
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(probeCtx, bin, "--version")
	cmd.Env = []string{"MISE_OFFLINE=1"}
	out, err := cmd.Output()
	if err != nil {
		return "", false
	}
	ver := parseMiseVersionOutput(string(out))
	if ver == "" {
		return "", false
	}
	return ver, true
}

func parseMiseVersionOutput(raw string) string {
	for line := range strings.SplitSeq(raw, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) == 0 {
			continue
		}
		tok := fields[0]
		if strings.EqualFold(tok, "mise") && len(fields) > 1 {
			tok = fields[1]
		}
		tok = strings.TrimPrefix(tok, "v")
		if tok != "" && tok[0] >= '0' && tok[0] <= '9' {
			return tok
		}
	}
	return ""
}

func downloadAndInstallMise(ctx context.Context, cfg miseBootstrapConfig, assetName, pinnedSum, bin string, logOut io.Writer) error {
	if len(cfg.baseURLs) == 0 {
		return errors.New("no release download URLs configured")
	}
	var errs []error
	for _, base := range cfg.baseURLs {
		base = strings.TrimRight(base, "/")
		wantSum := pinnedSum
		if wantSum == "" {
			var err error
			wantSum, err = fetchAssetSHA256(ctx, cfg.client, base+"/SHASUMS256.txt", assetName)
			if err != nil {
				errs = append(errs, err)
				if ctx.Err() != nil {
					return ctx.Err()
				}
				continue
			}
		}
		rawURL := base + "/" + assetName
		if err := fetchVerifyAndExtractMise(ctx, cfg.client, cfg.version, rawURL, assetName, wantSum, bin, logOut); err != nil {
			errs = append(errs, err)
			if ctx.Err() != nil {
				return ctx.Err()
			}
			continue
		}
		_, _ = fmt.Fprintf(logOut, "bootstrapped mise v%s from release archive %s -> %s\n", cfg.version, assetName, bin)
		return nil
	}
	return errors.Join(errs...)
}

func fetchAssetSHA256(ctx context.Context, client *zhttp.Client, shasumsURL, assetName string) (string, error) {
	var buf bytes.Buffer
	if _, err := client.Download(ctx, shasumsURL, maxMiseShasumsBytes, &buf); err != nil {
		return "", fmt.Errorf("fetch %s: %w", shasumsURL, err)
	}
	return parseShasums256(buf.String(), assetName)
}

func parseShasums256(manifest, assetName string) (string, error) {
	for line := range strings.SplitSeq(manifest, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) < 2 {
			continue
		}
		name := strings.TrimPrefix(strings.TrimPrefix(fields[1], "*"), "./")
		if name != assetName {
			continue
		}
		sum := strings.ToLower(fields[0])
		if len(sum) != 64 {
			return "", fmt.Errorf("invalid sha256 length for %s in SHASUMS256.txt: %q", assetName, fields[0])
		}
		if _, err := hex.DecodeString(sum); err != nil {
			return "", fmt.Errorf("invalid sha256 hex for %s in SHASUMS256.txt: %w", assetName, err)
		}
		return sum, nil
	}
	return "", fmt.Errorf("asset %s not found in SHASUMS256.txt", assetName)
}

func fetchVerifyAndExtractMise(ctx context.Context, client *zhttp.Client, version, rawURL, assetName, wantSum, bin string, logOut io.Writer) error {
	_, _ = fmt.Fprintf(logOut, "downloading mise v%s from %s\n", version, rawURL)

	tmpArchive, err := zfs.CreateTemp(filepath.Dir(bin), ".mise-archive.*.tar.gz")
	if err != nil {
		return err
	}
	archivePath := tmpArchive.Name()
	defer func() {
		_ = tmpArchive.Close()
		_ = zfs.RemoveIfPresent(archivePath)
	}()

	pw := &progressHashWriter{
		dst:      tmpArchive,
		hasher:   sha256.New(),
		logOut:   logOut,
		label:    assetName,
		nextMark: progressStepBytes,
	}
	if _, err := client.Download(ctx, rawURL, maxMiseArchiveBytes, pw); err != nil {
		return err
	}

	gotSum := hex.EncodeToString(pw.hasher.Sum(nil))
	if gotSum != wantSum {
		return fmt.Errorf("sha256 mismatch for %s: got %s, want %s", assetName, gotSum, wantSum)
	}

	if _, err := tmpArchive.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("seek archive: %w", err)
	}
	return extractMiseTarGz(tmpArchive, bin)
}

func extractMiseTarGz(r io.Reader, dstBin string) error {
	gzr, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("gzip reader: %w", err)
	}
	defer gzr.Close()

	tr := tar.NewReader(gzr)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read tar: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		clean := strings.TrimPrefix(hdr.Name, "./")
		if clean != "mise/bin/mise" {
			continue
		}
		if hdr.Size > maxMiseBinaryBytes {
			return fmt.Errorf("mise binary in archive exceeds limit (%d > %d)", hdr.Size, maxMiseBinaryBytes)
		}
		return zfs.AtomicWriteExecutable(dstBin, io.LimitReader(tr, maxMiseBinaryBytes))
	}
	return errors.New("archive did not contain mise/bin/mise")
}

func cargoInstallMise(ctx context.Context, cfg miseBootstrapConfig, zordonHome, bin string, logOut io.Writer) error {
	lookPath := cfg.lookPath
	if lookPath == nil {
		lookPath = exec.LookPath
	}
	cargoPath, err := lookPath("cargo")
	if err != nil {
		return fmt.Errorf("cargo not found on PATH (install cargo to compile mise v%s from source)", cfg.version)
	}
	lim := cfg.limiter
	if lim == nil {
		lim = &buildquota.Limiter{}
	}
	release, quota, err := lim.AcquireWithPhase(ctx, cfg.setPhase, "waiting for build slot", "installing mise")
	if err != nil {
		return err
	}
	defer release()

	_, _ = fmt.Fprintf(logOut, "compiling mise v%s from source via cargo install --locked --version %s mise\n", cfg.version, cfg.version)
	cmd := exec.Command(cargoPath, "install", "--root", zordonHome, "--locked", "--force", "--version", cfg.version, "mise")
	cmd.Env = buildquota.WithBuildEnv(withoutMakeJobserverEnv(zenv.Environ()), quota)
	cmd.Stdout = logOut
	cmd.Stderr = logOut
	if err := daemon.RunGroup(ctx, 2*time.Second, cmd); err != nil {
		if _, cmakeErr := lookPath("cmake"); cmakeErr != nil {
			return fmt.Errorf("cargo install mise@%s: %w (cmake was not found on PATH and mise native dependencies require it)", cfg.version, err)
		}
		return fmt.Errorf("cargo install mise@%s: %w", cfg.version, err)
	}
	_, _ = fmt.Fprintf(logOut, "compiled mise v%s from source -> %s\n", cfg.version, bin)
	return nil
}

type progressHashWriter struct {
	dst      io.Writer
	hasher   hash.Hash
	logOut   io.Writer
	label    string
	written  int64
	nextMark int64
}

func (p *progressHashWriter) Write(b []byte) (int, error) {
	n, err := p.dst.Write(b)
	if n > 0 {
		_, _ = p.hasher.Write(b[:n])
		p.written += int64(n)
		for p.written >= p.nextMark {
			_, _ = fmt.Fprintf(p.logOut, "downloading %s: %.1f MiB...\n", p.label, float64(p.nextMark)/(1<<20))
			p.nextMark += progressStepBytes
		}
	}
	return n, err
}
