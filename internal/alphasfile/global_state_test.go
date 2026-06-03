package alphasfile

import (
	"reflect"
	"testing"
)

func svc(tc, name string) *Service {
	return &Service{Toolchain: tc, Runtime: &RuntimeConfig{Name: name}}
}

// Zero value is the empty parent context the chain root sees: no construction,
// no parent context, no accumulated facts.
func TestGlobalComputedState_ZeroValue(t *testing.T) {
	var g GlobalComputedState
	if g.ParentContext() != nil {
		t.Error("zero value ParentContext() = non-nil, want nil")
	}
	if g.Services() != nil {
		t.Errorf("zero value Services() = %v, want nil", g.Services())
	}
	if g.Dotenv() != nil {
		t.Errorf("zero value Dotenv() = %v, want nil", g.Dotenv())
	}
}

// Join preserves the four chain merge rules: services append, toolchain merges
// with the later (child) entry winning, sysenv unions order-preserving, dotenv
// appends root-first.
func TestGlobalComputedState_Join(t *testing.T) {
	var g GlobalComputedState
	g.Join(Contribution{
		Services:  []*Service{svc("go", "a")},
		Toolchain: map[string]*ToolchainConfig{"go": {Version: "1.21"}},
		SysEnv:    []string{"PATH", "HOME"},
		Dotenv:    []string{".env"},
	})
	g.Join(Contribution{
		Services:  []*Service{svc("rust", "b")},
		Toolchain: map[string]*ToolchainConfig{"go": {Version: "1.22"}, "rust": {Version: "1.79"}},
		SysEnv:    []string{"HOME", "TERM"},
		Dotenv:    []string{".env.local"},
	})

	var gotNames []string
	for _, s := range g.Services() {
		gotNames = append(gotNames, s.Name())
	}
	if want := []string{"a", "b"}; !reflect.DeepEqual(gotNames, want) {
		t.Errorf("Services order = %v, want %v", gotNames, want)
	}

	if want := []string{".env", ".env.local"}; !reflect.DeepEqual(g.Dotenv(), want) {
		t.Errorf("Dotenv() = %v, want %v", g.Dotenv(), want)
	}

	pc := g.ParentContext()
	if pc == nil {
		t.Fatal("ParentContext() = nil after Join, want non-nil")
	}
	if want := []string{"PATH", "HOME", "TERM"}; !reflect.DeepEqual(pc.SysEnv(), want) {
		t.Errorf("SysEnv() = %v, want %v (order-preserving union)", pc.SysEnv(), want)
	}
	tc := pc.Toolchain()
	if tc["go"].Version != "1.22" {
		t.Errorf("toolchain go = %q, want 1.22 (later/child wins)", tc["go"].Version)
	}
	if tc["rust"].Version != "1.79" {
		t.Errorf("toolchain rust = %q, want 1.79", tc["rust"].Version)
	}
}

// A dotenv-only contribution must not make ParentContext non-nil: dotenv flows
// separately (parentDotenv), exactly as the pre-refactor guard
// `if len(accumulated) > 0 || len(toolchain) > 0 || len(sysenv) > 0` did.
func TestGlobalComputedState_ParentContextNilWithoutEvalFacts(t *testing.T) {
	var g GlobalComputedState
	g.Join(Contribution{Dotenv: []string{".env"}})
	if g.ParentContext() != nil {
		t.Error("ParentContext() non-nil after dotenv-only Join, want nil")
	}
	if want := []string{".env"}; !reflect.DeepEqual(g.Dotenv(), want) {
		t.Errorf("Dotenv() = %v, want %v", g.Dotenv(), want)
	}
}
