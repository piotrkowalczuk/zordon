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
// mise's pin actually takes effect at the language-runtime level and
// to silence interactive prompts that would deadlock under alpha's
// piped stdin. Mise pins which VERSION of node/go/ruby/rust is on
// PATH; PostMiseEnv pins how the language's own machinery behaves so
// the pinned version isn't silently swapped by an auto-switch (Go's
// GOTOOLCHAIN, Node's Corepack download prompt) and so per-PM
// banners (npm fund/audit) don't litter the log.
//
// Each language's adjustments live in this file in a tiny per-lang
// function so the language-specific knowledge stays together — and
// callers (alpha) stay language-agnostic.
func PostMiseEnv(lang string, env map[string]string) {
	switch lang {
	case "go":
		postMiseGo(env)
	case "nodejs":
		postMiseNode(env)
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

// postMiseNode silences npm's per-install banners and Corepack's
// download prompt. The PATH side of the pin is already handled by
// `mise env --json node@<ver>` (node bin dir) and EnsureNodeCorepack
// (pnpm/yarn shim dir on top). Knobs:
//
//   - NPM_CONFIG_FUND=false / NPM_CONFIG_AUDIT=false: drop npm's
//     "consider funding ..." / vulnerability-count banner on every
//     install. Pure log hygiene — install behavior unchanged.
//   - COREPACK_ENABLE_DOWNLOAD_PROMPT=0: Corepack defaults to printing
//     "About to download X, [Y/n]?" the first time it provisions a PM
//     — fine interactively, deadlocks under alpha's piped stdin.
//
// `if not set` semantics so a power-user's toolchain.nodejs.env can
// still override.
func postMiseNode(env map[string]string) {
	if env["NPM_CONFIG_FUND"] == "" {
		env["NPM_CONFIG_FUND"] = "false"
	}
	if env["NPM_CONFIG_AUDIT"] == "" {
		env["NPM_CONFIG_AUDIT"] = "false"
	}
	if env["COREPACK_ENABLE_DOWNLOAD_PROMPT"] == "" {
		env["COREPACK_ENABLE_DOWNLOAD_PROMPT"] = "0"
	}
}
