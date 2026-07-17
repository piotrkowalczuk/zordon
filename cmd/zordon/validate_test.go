package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/piotrkowalczuk/zordon/internal/alphasfile"
	"github.com/piotrkowalczuk/zordon/internal/zfs"
)

// validateInPlaceSources is the plan/start preflight that reports a removed
// in-place src{exe} subdir as a clean config error (issue #44), before a
// build/runtime spawn misreports it as fork/exec <toolchain binary>. It only
// fires when the checkout root exists but the exe-anchored dir does not.
func TestValidateInPlaceSources(t *testing.T) {
	root := t.TempDir()
	present := filepath.Join(root, "cmd", "here")
	if err := zfs.EnsureDir(present); err != nil {
		t.Fatalf("mkdir fixture: %v", err)
	}
	missing := filepath.Join(root, "cmd", "gone")

	inPlace := func(name, exe, checkout, dir string) *alphasfile.Service {
		return &alphasfile.Service{
			Toolchain: alphasfile.ToolchainGo,
			Package:   &alphasfile.Package{Toolchain: alphasfile.ToolchainGo, Src: checkout, Exe: exe, InPlace: true},
			Runtime:   &alphasfile.RuntimeConfig{Name: name, Checkout: checkout, Dir: dir},
		}
	}

	t.Run("missing exe subdir errors", func(t *testing.T) {
		af := &alphasfile.Alphasfile{Services: []*alphasfile.Service{
			inPlace("api", "./cmd/gone", root, missing),
		}}
		err := validateInPlaceSources(af)
		if err == nil {
			t.Fatal("want error for removed in-place exe subdir, got nil")
		}
		for _, w := range []string{`"api"`, `"./cmd/gone"`, "does not exist"} {
			if !strings.Contains(err.Error(), w) {
				t.Errorf("error %q missing substring %q", err.Error(), w)
			}
		}
		if strings.Contains(err.Error(), "fork/exec") {
			t.Errorf("error %q must not mention fork/exec", err.Error())
		}
	})

	t.Run("present exe subdir passes", func(t *testing.T) {
		af := &alphasfile.Alphasfile{Services: []*alphasfile.Service{
			inPlace("api", "./cmd/here", root, present),
		}}
		if err := validateInPlaceSources(af); err != nil {
			t.Fatalf("want nil for existing exe subdir, got %v", err)
		}
	})

	t.Run("missing checkout root is skipped", func(t *testing.T) {
		absent := filepath.Join(root, "not-a-checkout")
		af := &alphasfile.Alphasfile{Services: []*alphasfile.Service{
			inPlace("api", "./cmd/gone", absent, filepath.Join(absent, "cmd", "gone")),
		}}
		if err := validateInPlaceSources(af); err != nil {
			t.Fatalf("missing root is a src.path concern, not exe; want nil, got %v", err)
		}
	})

	t.Run("non in-place service is skipped", func(t *testing.T) {
		git := &alphasfile.Service{
			Toolchain: alphasfile.ToolchainGo,
			Package:   &alphasfile.Package{Toolchain: alphasfile.ToolchainGo, Git: "github.com/acme/api", Exe: "./cmd/gone", InPlace: false},
			Runtime:   &alphasfile.RuntimeConfig{Name: "api", Checkout: root, Dir: missing},
		}
		af := &alphasfile.Alphasfile{Services: []*alphasfile.Service{git}}
		if err := validateInPlaceSources(af); err != nil {
			t.Fatalf("git primary is materialized at bringup, not checkable here; want nil, got %v", err)
		}
	})
}
