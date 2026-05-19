package tools

// PostMiseEnv applies language-specific env adjustments on top of
// mise's `env --json` output, BEFORE the user's toolchain.<lang>.env
// overlay. Layered model:
//
//   1. sysenv whitelist  — filters host env
//   2. mise env --json   — toolchain base (PATH, GOROOT, GEM_PATH, ...)
//   3. PostMiseEnv       — language pin reinforcement (this function)
//   4. toolchain.<lang>.env  — user override (power-user path, no safety net)
//
// Layer 3 exists for ONE narrow purpose per language: to make sure
// mise's pin actually takes effect at the language-runtime level.
// Mise pins which VERSION of go/ruby/rust is on PATH; PostMiseEnv
// pins how the language's own auto-switch / re-execution machinery
// behaves so the pinned version isn't silently swapped by the
// language's own logic. For Go that means GOTOOLCHAIN=local; other
// languages may have analogous knobs (e.g. RUSTUP_TOOLCHAIN, none
// yet for Ruby).
//
// Each language's adjustments live in this file in a tiny per-lang
// function so the language-specific knowledge stays together — and
// callers (alpha) stay language-agnostic.
func PostMiseEnv(lang string, env map[string]string) {
	switch lang {
	case "go":
		postMiseGo(env)
	}
}

// postMiseGo sets GOTOOLCHAIN=local when mise / sysenv / user
// haven't already pinned it. Without this, Go's default `auto`
// reads each project's go.mod / go.work `toolchain` directive and
// auto-downloads whatever version it requests — defeating the
// zordon-Alphasfile pin. With `local`, Go uses the version it was
// invoked as (i.e. the mise-pinned one) and fails loudly if a
// go.mod requires more.
//
// `if not set` semantics — the user's Alphasfile toolchain.go.env
// or per-service env can still override (power-user path: explicit
// `GOTOOLCHAIN = "auto"` for a project that legitimately wants
// auto-switch).
func postMiseGo(env map[string]string) {
	if env["GOTOOLCHAIN"] == "" {
		env["GOTOOLCHAIN"] = "local"
	}
}
