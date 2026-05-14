package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/codegangsta/cli"
	"go.uber.org/zap"
)

func recruit(ctx *cli.Context) error {
	af, err := openAlphasfile(alphasFile)
	if err != nil {
		logger.Fatal(err.Error())
	}

	logger.Warn("Alpha, Rita's escaped! Recruit a team of services with attitude!", zap.String(keySubsystem, "zordon"))
	logger.Info("Understood, Zordon!", zap.String(keySubsystem, "alpha"))

	all := af.All()
	toolchains := map[string]bool{}
	for _, s := range all {
		rl := serviceLogger(logger, s)
		var src sourceInfo
		if s.NeedsSource() {
			var err error
			src, err = ensureSource(s, rl)
			if err != nil {
				logger.Fatal("Ayiyiyiyi!", zap.String(keySubsystem, "alpha"), zap.Error(err))
			}
		}
		if err := run(s.InstallCmd(src), s, rl); err != nil {
			logger.Fatal("Ayiyiyiyi!", zap.String(keySubsystem, "alpha"), zap.Error(err))
		}

		toolchains[s.Toolchain] = true
		logger.Info(fmt.Sprintf("%s ready", s.Name), zap.String(keySubsystem, "alpha"), zap.String("branch", s.Branch()), zap.String("toolchain", s.Toolchain))
	}

	warnIfBinNotInPath(toolchains)
	logger.Info("Use extreme caution Rangers, you are dealing with an evil here that is beyond all imagination!", zap.String(keySubsystem, "zordon"))
	return nil
}

type sourceInfo struct {
	repoURL string
	repoDir string
	subpath string
}

func resolveSource(s *Service) (sourceInfo, error) {
	src := s.Source()
	parts := strings.Split(src, "/")
	if len(parts) < 3 {
		return sourceInfo{}, fmt.Errorf("import path too short: %q", src)
	}
	host := parts[0]
	switch host {
	case "github.com", "gitlab.com", "bitbucket.org":
	default:
		return sourceInfo{}, fmt.Errorf("unsupported import host %q (only github.com, gitlab.com, bitbucket.org for now)", host)
	}
	repoImport := strings.Join(parts[:3], "/")
	remaining := parts[3:]
	if len(remaining) > 0 && isMajorVersionMarker(remaining[0]) {
		remaining = remaining[1:]
	}
	sub := "."
	if len(remaining) > 0 {
		sub = "./" + strings.Join(remaining, "/")
	}
	return sourceInfo{
		repoURL: "https://" + repoImport + ".git",
		repoDir: filepath.Join(zordonHome(), "src", repoImport),
		subpath: sub,
	}, nil
}

func isMajorVersionMarker(seg string) bool {
	if len(seg) < 2 || seg[0] != 'v' {
		return false
	}
	for _, r := range seg[1:] {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func zordonHome() string {
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".zordon")
	}
	return ".zordon"
}

func ensureSource(s *Service, l *zap.Logger) (sourceInfo, error) {
	src, err := resolveSource(s)
	if err != nil {
		return src, err
	}
	branch := s.Branch()
	if _, err := os.Stat(src.repoDir); os.IsNotExist(err) {
		if err := os.MkdirAll(filepath.Dir(src.repoDir), 0o755); err != nil {
			return src, err
		}
		args := []string{"clone", "--depth", "1", "--recurse-submodules"}
		if branch != "" {
			args = append(args, "--branch", branch)
		}
		args = append(args, src.repoURL, src.repoDir)
		if err := run(exec.Command("git", args...), s, l); err != nil {
			return src, err
		}
		return src, nil
	} else if err != nil {
		return src, err
	}

	fetchArgs := []string{"-C", src.repoDir, "fetch", "--depth", "1", "origin"}
	if branch != "" {
		fetchArgs = append(fetchArgs, branch)
	}
	if err := run(exec.Command("git", fetchArgs...), s, l); err != nil {
		return src, err
	}
	if err := run(exec.Command("git", "-C", src.repoDir, "reset", "--hard", "FETCH_HEAD"), s, l); err != nil {
		return src, err
	}
	return src, nil
}

func goBinDir() string {
	if out, err := exec.Command("go", "env", "GOBIN").Output(); err == nil {
		if dir := strings.TrimSpace(string(out)); dir != "" {
			return dir
		}
	}
	if out, err := exec.Command("go", "env", "GOPATH").Output(); err == nil {
		if dir := strings.TrimSpace(string(out)); dir != "" {
			return filepath.Join(dir, "bin")
		}
	}
	return ""
}

func cargoBinDir() string {
	if dir := os.Getenv("CARGO_HOME"); dir != "" {
		return filepath.Join(dir, "bin")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".cargo", "bin")
}

func warnIfBinNotInPath(toolchains map[string]bool) {
	pathDirs := filepath.SplitList(os.Getenv("PATH"))
	check := func(bin string) {
		if bin == "" || slices.Contains(pathDirs, bin) {
			return
		}
		logger.Warn(fmt.Sprintf("%s is not in $PATH — installed service binaries will not be found", bin), zap.String(keySubsystem, "zordon"))
	}
	if toolchains[ToolchainGo] {
		check(goBinDir())
	}
	if toolchains[ToolchainRust] {
		check(cargoBinDir())
	}
}
