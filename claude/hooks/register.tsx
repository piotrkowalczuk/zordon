import { atom, read, update } from 'claude-code'
import type { EngineInterface, Register, RenderChildren, UiOpenResult } from 'claude-code'

import { registerDiff } from './diff'
import type {
  Checkout,
  McpCall,
  UpdateAction,
  UpdateInfo,
  UpdateMethod,
  ZordonService,
  ZordonSnapshot,
  ZordonWorkspace,
} from '../types'

export const PLUGIN_VERSION = '0.29.1'

export type Semver = {
  major: number
  minor: number
  patch: number
  pre: string | null
}

const POLL_MS = 5000
const SIZE_EVERY_MS = 60_000
const VERSION_EVERY_MS = 60_000
const UPDATE_EVERY_MS = 6 * 60 * 60_000
const UPDATE_RETRY_MS = 15 * 60_000
const RELEASE_API_URL = 'https://api.github.com/repos/piotrkowalczuk/zordon/releases/latest'
const RELEASE_TAG_BASE = 'https://github.com/piotrkowalczuk/zordon/releases/tag/v'
const CLAUDE_PLUGIN_UPGRADE_CMD = 'claude plugin update zordon@zordon'
const BREW_UPGRADE_CMD = 'brew upgrade piotrkowalczuk/tap/zordon'
const UPGRADE_TARBALL_SH = `
ver="$1"
os="$2"
arch="$3"
dest="$4"
base="\${5:-https://github.com/piotrkowalczuk/zordon/releases/download}"
if [ -z "$dest" ] || [ ! -d "$dest" ] || [ ! -w "$dest" ]; then
  dest="$HOME/.local/bin"
  mkdir -p "$dest"
fi
stage="$(mktemp -d "$dest/.zordon-upgrade.XXXXXX")"
trap 'rm -rf "$stage"' EXIT
archive="zordon_\${ver}_\${os}_\${arch}.tar.gz"
curl -fsSL --max-time 30 "\${base}/v\${ver}/checksums.txt" -o "$stage/checksums.txt"
expected="$(awk -v f="$archive" '$2 == f || $2 == "*"f { print tolower($1); exit }' "$stage/checksums.txt")"
case "$expected" in
  [0-9a-f][0-9a-f]*) ;;
  *) echo "checksum not found for $archive" >&2; exit 1 ;;
esac
if [ "\${#expected}" -ne 64 ]; then
  echo "invalid checksum length for $archive" >&2
  exit 1
fi
curl -fsSL --max-time 120 "\${base}/v\${ver}/\${archive}" -o "$stage/$archive"
if command -v sha256sum >/dev/null 2>&1; then
  actual="$(sha256sum "$stage/$archive" | awk '{ print tolower($1) }')"
else
  actual="$(shasum -a 256 "$stage/$archive" | awk '{ print tolower($1) }')"
fi
if [ "$actual" != "$expected" ]; then
  echo "SHA-256 mismatch for $archive" >&2
  exit 1
fi
tar -xzf "$stage/$archive" -C "$stage" zordon alpha tommy
for bin in tommy alpha zordon; do
  if [ ! -f "$stage/$bin" ] || [ -L "$stage/$bin" ]; then
    echo "archive missing regular file $bin" >&2
    exit 1
  fi
  chmod 0755 "$stage/$bin"
done
if [ "$os" = "darwin" ] && command -v xattr >/dev/null 2>&1; then
  xattr -dr com.apple.quarantine "$stage/tommy" "$stage/alpha" "$stage/zordon" 2>/dev/null || true
fi
for bin in tommy alpha zordon; do
  mv -f "$stage/$bin" "$dest/$bin"
done
printf '%s\\n' "$dest"
`

const snapshot = atom(
  { plugin: 'zordon', key: 'snapshot' } as const,
  { kind: 'loading' } as ZordonSnapshot,
)
const isCollapsed = atom({ plugin: 'zordon', key: 'isCollapsed' } as const, false)
const isScopeCollapsed = atom({ plugin: 'zordon', key: 'isScopeCollapsed' } as const, false)
const isRuntimeCollapsed = atom({ plugin: 'zordon', key: 'isRuntimeCollapsed' } as const, false)
const isLogCollapsed = atom({ plugin: 'zordon', key: 'isLogCollapsed' } as const, false)
const calls = atom({ plugin: 'zordon', key: 'calls' } as const, [] as McpCall[])
const checkouts = atom({ plugin: 'zordon', key: 'checkouts' } as const, [] as Checkout[])
// The Runtime row opened to its details: lines are cut to the box's width,
// and a service's details show its print and the rest whole, wrapped.
const openService = atom({ plugin: 'zordon', key: 'openService' } as const, null as string | null)
const openCall = atom({ plugin: 'zordon', key: 'openCall' } as const, null as string | null)
const updateInfo = atom({ plugin: 'zordon', key: 'update' } as const, null as UpdateInfo | null)
const dismissedUpdate = atom({ plugin: 'zordon', key: 'dismissedUpdate' } as const, null as string | null)
const updateAction = atom(
  { plugin: 'zordon', key: 'updateAction' } as const,
  { state: 'idle', message: null } as UpdateAction,
)

let sessionCwd = '.'

// The zordon MCP tools: this plugin's own server (mcp__plugin_zordon_zordon__*)
// or one the person configured as `zordon` (mcp__zordon__*).
const MCP_TOOL = /^mcp__(?:plugin_zordon_)?zordon__(.+)$/
const MAX_CALLS = 30
const MAX_OUTPUT = 4000

const PANE = 'zordon'
const DASHBOARD = 'zordon:dashboard'

async function current($: EngineInterface): Promise<ZordonSnapshot> {
  return read($, snapshot)
}

async function openPane($: EngineInterface, focus: boolean): Promise<UiOpenResult> {
  return $.ui.open({ id: PANE, title: 'zordon', columns: 56, ...(focus ? { focus: true as const } : {}) })
}

const HEADER_LINE = /^# \[[0-9a-f]+\] (.+?) \(invocation, workspace=([^)]+)\)$/
const SERVICE_LINE = /^\s+- \[([^\]]*)\] (.+?) — (.+)$/
const DETAIL_LINE = /^ {8}(.*)$/
const ALPHA_LINE = /^\s+alpha pid=(\d+)/
const ERROR_LINE = /\[ERROR\]\s*-?\s*(.*)$/

function dirname(path: string): string {
  return path.slice(0, path.lastIndexOf('/')) || '/'
}

function parseWorkspace(stdout: string): ZordonWorkspace | null {
  for (const line of stdout.split('\n')) {
    const m = HEADER_LINE.exec(line)
    if (m) {
      const [, alphasfile = '', name = 'main'] = m
      return { name, alphasfile, stateDir: `${dirname(alphasfile)}/workspaces/${name}`, sizeKB: null }
    }
  }

  return null
}

const LEVEL_LINE = /^# \[[0-9a-f]+\] /
const CHECKOUT_LINE = /^checkout: (.+?) \((?:branch (\S+)[^)]*|detached @ [^)]+)\)$/

// A service is picked (in the workspace's scope) when its checkout is the
// workspace's own worktree, workspaces/<ws>/src/<svc>; main picks nothing.
function isPickedCheckout(checkout: string | null, name: string, workspace: string): boolean {
  if (checkout === null || workspace === 'main') return false

  return checkout === `src/${name}` || checkout.endsWith(`/workspaces/${workspace}/src/${name}`)
}

function parseServices(stdout: string): { services: ZordonService[]; isAlphaUp: boolean } {
  const services: ZordonService[] = []
  let isAlphaUp = false
  let isLeaf = false

  for (const line of stdout.split('\n')) {
    if (LEVEL_LINE.test(line)) {
      isLeaf = HEADER_LINE.test(line)
      continue
    }

    const alpha = ALPHA_LINE.exec(line)
    if (alpha) {
      isAlphaUp ||= isLeaf && Number(alpha[1] ?? 0) > 0
      continue
    }

    const svc = SERVICE_LINE.exec(line)
    if (svc) {
      const [, toolchain = '', name = '', rawState = ''] = svc
      const state = rawState.trim()
      const isFailed = state === 'failed' || state.includes('[unhealthy')
      services.push({
        toolchain,
        name,
        state,
        isFailed,
        isUp: state.startsWith('running') && state.includes('[ready]'),
        print: [],
        checkout: null,
        branch: null,
        sourceDir: null,
        isShared: !isLeaf,
        isPicked: false,
      })
      continue
    }

    const text = DETAIL_LINE.exec(line)?.[1]
    const last = services[services.length - 1]
    if (text === undefined || !last) continue
    const co = CHECKOUT_LINE.exec(text)
    if (co) {
      last.checkout = co[1] ?? null
      last.branch = co[2] ?? null
    } else if (!text.startsWith('checkout:')) {
      last.print.push(text)
    }
  }

  return { services, isAlphaUp }
}

export function parseStatus(exitCode: number, stdout: string, stderr: string, cwd = '/'): ZordonSnapshot {
  const all = `${stdout}\n${stderr}`
  if (all.includes('no Alphasfile')) {
    return { kind: 'inactive' }
  }

  const workspace = parseWorkspace(stdout)
  const parsed = parseServices(stdout)
  const isAlphaUp = parsed.isAlphaUp
  // The text report writes a checkout beneath the working directory relative to it.
  const absolute = (path: string | null) => (path === null || path.startsWith('/') ? path : `${cwd.replace(/\/$/, '')}/${path}`)
  const services = parsed.services.map(svc => ({
    ...svc,
    checkout: absolute(svc.checkout),
    isPicked: !svc.isShared && isPickedCheckout(svc.checkout, svc.name, workspace?.name ?? 'main'),
  }))
  if (exitCode !== 0 && !all.includes('no alpha running')) {
    const errors = stderr
      .split('\n')
      .map(l => ERROR_LINE.exec(l)?.[1]?.trim())
      .filter((l): l is string => !!l)
    const fallback = stderr.trim().split('\n').pop() || `zordon status exited ${exitCode}`

    return { kind: 'error', message: errors.length > 0 ? errors.join('\n') : fallback, workspace }
  }

  return { kind: isAlphaUp ? 'running' : 'stopped', services, workspace }
}

type AgentService = {
  name: string
  state: string
  health?: string
  picked?: boolean
  shared?: boolean
  print?: string
  checkout?: string
  checkout_path?: string
  branch?: string
  source_dir?: string
}

type AgentStatus = {
  workspace?: string
  alphasfile?: string
  state_dir?: string
  state: string
  error?: string
  services?: AgentService[]
}

// `zordon status --format=json`: one JSON object; null when this zordon predates it
// and printed its text report instead.
export function parseAgentStatus(stdout: string): ZordonSnapshot | null {
  let st: AgentStatus
  try {
    st = JSON.parse(stdout) as AgentStatus
  } catch {
    return null
  }
  if (typeof st !== 'object' || st === null || typeof st.state !== 'string') return null

  if (st.state === 'no-alphasfile') return { kind: 'inactive' }
  const workspace: ZordonWorkspace | null =
    st.workspace && st.alphasfile && st.state_dir
      ? { name: st.workspace, alphasfile: st.alphasfile, stateDir: st.state_dir, sizeKB: null }
      : null
  if (st.state === 'error') return { kind: 'error', message: st.error ?? 'zordon failed', workspace }

  const services = (st.services ?? []).map(
    (x): ZordonService => ({
      name: x.name,
      toolchain: '',
      state: x.health ? `${x.state}: ${x.health}` : x.state,
      isUp: x.state === 'ready',
      isFailed: x.state === 'failed' || x.state === 'unhealthy',
      print: x.print ? [x.print] : [],
      checkout: x.checkout_path ?? null,
      branch: x.branch ?? null,
      sourceDir: x.source_dir ?? null,
      isShared: x.shared === true,
      isPicked: x.picked === true,
    }),
  )

  return { kind: st.state === 'running' ? 'running' : 'stopped', services, workspace }
}

export function formatSize(kb: number): string {
  const units = ['KB', 'MB', 'GB', 'TB']
  let value = kb
  let unit = 0
  while (value >= 1024 && unit < units.length - 1) {
    value /= 1024
    unit += 1
  }

  return `${value < 10 && unit > 0 ? value.toFixed(1) : Math.round(value)} ${units[unit]}`
}

export function parseSemver(raw: string): Semver | null {
  const m = /^v?(\d+)\.(\d+)\.(\d+)(?:-([0-9A-Za-z.-]+))?(?:\+[0-9A-Za-z.-]+)?$/.exec(raw.trim())
  if (!m) return null
  const major = Number(m[1])
  const minor = Number(m[2])
  const patch = Number(m[3])
  if (!Number.isSafeInteger(major) || !Number.isSafeInteger(minor) || !Number.isSafeInteger(patch)) return null

  return { major, minor, patch, pre: m[4] ?? null }
}

export function compareSemver(a: Semver, b: Semver): number {
  if (a.major !== b.major) return a.major - b.major
  if (a.minor !== b.minor) return a.minor - b.minor
  if (a.patch !== b.patch) return a.patch - b.patch
  if (a.pre === b.pre) return 0
  if (a.pre === null) return 1
  if (b.pre === null) return -1

  const ap = a.pre.split('.')
  const bp = b.pre.split('.')
  const len = Math.max(ap.length, bp.length)
  for (let i = 0; i < len; i += 1) {
    const ai = ap[i]
    const bi = bp[i]
    if (ai === undefined) return -1
    if (bi === undefined) return 1
    if (ai === bi) continue
    const aNum = /^\d+$/.test(ai)
    const bNum = /^\d+$/.test(bi)
    if (aNum && bNum) return Number(ai) - Number(bi)
    if (aNum) return -1
    if (bNum) return 1

    return ai < bi ? -1 : 1
  }

  return 0
}

export function parseZordonVersion(stdout: string): string | null {
  const first = stdout.split('\n').map(l => l.trim()).find(Boolean)
  if (!first) return null
  const m = /^zordon\s+(\S+)/.exec(first)
  if (!m?.[1]) return null
  const token = m[1]
  if (token === '(devel)') return '(devel)'

  return parseSemver(token) ? token.replace(/^v/, '') : null
}

export function parseLatestRelease(stdout: string): { version: string; url: string } | null {
  let obj: { tag_name?: unknown; html_url?: unknown; draft?: unknown; prerelease?: unknown }
  try {
    obj = JSON.parse(stdout) as typeof obj
  } catch {
    return null
  }
  if (typeof obj !== 'object' || obj === null || obj.draft === true || obj.prerelease === true) return null
  if (typeof obj.tag_name !== 'string') return null
  const sv = parseSemver(obj.tag_name)
  if (!sv || sv.pre !== null) return null
  const version = `${sv.major}.${sv.minor}.${sv.patch}`
  const fallbackUrl = new URL(`${RELEASE_TAG_BASE}${version}`).href
  if (typeof obj.html_url === 'string' && obj.html_url.startsWith('https://github.com/piotrkowalczuk/zordon/releases/')) {
    try {
      return { version, url: new URL(obj.html_url).href }
    } catch {
      return { version, url: fallbackUrl }
    }
  }

  return { version, url: fallbackUrl }
}

export function isBrewBinary(binPath: string | null, resolvedPath?: string | null | undefined): boolean {
  const combined = `${binPath ?? ''}\0${resolvedPath ?? ''}`.toLowerCase()

  return (
    combined.includes('homebrew') ||
    combined.includes('cellar') ||
    combined.includes('caskroom') ||
    combined.includes('linuxbrew')
  )
}

export function parseUname(stdout: string): { os: 'darwin' | 'linux'; arch: 'arm64' | 'amd64' } | null {
  const parts = stdout.trim().split(/\s+/)
  const rawOs = (parts[0] ?? '').toLowerCase()
  const rawArch = (parts[1] ?? '').toLowerCase()
  const os = rawOs === 'darwin' ? 'darwin' : rawOs === 'linux' ? 'linux' : null
  const arch =
    rawArch === 'arm64' || rawArch === 'aarch64' ? 'arm64' : rawArch === 'x86_64' || rawArch === 'amd64' ? 'amd64' : null
  if (!os || !arch) return null

  return { os, arch }
}

export function binaryUpgradePlan(
  binPath: string | null,
  resolvedPath?: string | null | undefined,
): { method: UpdateMethod; targetDir: string; label: string } {
  const targetDir = binPath ? dirname(binPath) : ''
  if (isBrewBinary(binPath, resolvedPath)) {
    return { method: 'brew', targetDir, label: BREW_UPGRADE_CMD }
  }
  const shownDir = targetDir || '~/.local/bin'

  return { method: 'download', targetDir, label: `download release → ${shownDir}` }
}

export function computeUpdate(opts: {
  binaryVersion: string | null
  pluginVersion: string
  latestRelease: { version: string; url: string } | null
  binPath?: string | null | undefined
  resolvedBinPath?: string | null | undefined
  pluginUpgradeCmd?: string | undefined
}): UpdateInfo | null {
  const pluginSv = parseSemver(opts.pluginVersion)
  if (!pluginSv) return null

  const binSv = opts.binaryVersion && opts.binaryVersion !== '(devel)' ? parseSemver(opts.binaryVersion) : null
  const relSv = opts.latestRelease ? parseSemver(opts.latestRelease.version) : null

  const isBinaryOutdated = binSv !== null && relSv !== null && compareSemver(binSv, relSv) < 0

  let pluginTargetSv: Semver | null = relSv
  if (binSv !== null && binSv.pre === null && (pluginTargetSv === null || compareSemver(binSv, pluginTargetSv) > 0)) {
    pluginTargetSv = binSv
  }
  const isPluginOutdated = pluginTargetSv !== null && compareSemver(pluginSv, pluginTargetSv) < 0

  if (!isBinaryOutdated && !isPluginOutdated) return null

  const targetSv =
    relSv !== null && (pluginTargetSv === null || compareSemver(relSv, pluginTargetSv) >= 0) ? relSv : pluginTargetSv
  if (!targetSv) return null

  const latest = `${targetSv.major}.${targetSv.minor}.${targetSv.patch}`
  const releaseUrl =
    opts.latestRelease && opts.latestRelease.version === latest
      ? new URL(opts.latestRelease.url).href
      : new URL(`${RELEASE_TAG_BASE}${latest}`).href
  const plan = binaryUpgradePlan(opts.binPath ?? null, opts.resolvedBinPath ?? null)

  return {
    latest,
    releaseUrl,
    binaryVersion: opts.binaryVersion,
    pluginVersion: opts.pluginVersion.replace(/^v/, ''),
    isBinaryOutdated,
    isPluginOutdated,
    binaryMethod: plan.method,
    binaryTargetDir: plan.targetDir,
    binaryUpgradeCmd: plan.label,
    pluginUpgradeCmd: opts.pluginUpgradeCmd ?? CLAUDE_PLUGIN_UPGRADE_CMD,
  }
}

export function updatePromptLine(u: UpdateInfo): string {
  if (u.isBinaryOutdated && u.isPluginOutdated) {
    return `↑ zordon v${u.latest} available (cli v${u.binaryVersion}, plugin ${u.pluginVersion})`
  }
  if (u.isBinaryOutdated) {
    return `↑ zordon v${u.latest} available (cli v${u.binaryVersion})`
  }

  return `↑ zordon plugin ${u.latest} available (installed ${u.pluginVersion})`
}

function summary(s: ZordonSnapshot): string | undefined {
  switch (s.kind) {
    case 'loading':
    case 'inactive':
      return undefined
    case 'error':
      return 'zordon ✗ error'
    case 'stopped':
      return 'zordon stopped'
    case 'running': {
      const up = s.services.filter(x => x.isUp).length
      const failed = s.services.filter(x => x.isFailed).length

      return `zordon ${up}/${s.services.length} up${failed > 0 ? ` · ${failed} failed` : ''}`
    }
  }
}

// One glyph per state, colored where drawn: the list stays quiet when all is
// well and a word appears only for what is not ready.
// Runtime's order: what needs a look first (failed or unhealthy), then what
// is coming up, then what runs, then what is stopped; by name within each.
// Read off the same flags as the dots, so a text report's states sort too.
function stateRank(x: ZordonService): number {
  if (x.isFailed) return 0
  if (x.isUp) return 2
  if (x.state === 'stopped') return 3

  return 1
}

export function byStateThenName(a: ZordonService, b: ZordonService): number {
  return stateRank(a) - stateRank(b) || a.name.localeCompare(b.name)
}

export function serviceMark(svc: ZordonService): { glyph: string; color: string; note: string | null } {
  if (svc.isFailed) return { glyph: '●', color: 'red', note: svc.state.includes('unhealthy') ? 'unhealthy' : 'failed' }
  if (svc.isUp) return { glyph: '●', color: 'green', note: null }
  if (svc.state === 'stopped') return { glyph: '○', color: 'gray', note: 'stopped' }

  return { glyph: '◐', color: 'yellow', note: svc.state.includes('probing') ? 'probing' : 'starting' }
}

// The services a workspace picked (its own worktrees) and the rest its run
// brings up: unpicked services and the federation's shared levels.
export function splitScope(services: ZordonService[]): { scope: ZordonService[]; runtime: ZordonService[] } {
  return { scope: services.filter(x => x.isPicked), runtime: services.filter(x => !x.isPicked) }
}

// Opens a file in the desktop's default app: open on macOS, xdg-open elsewhere.
async function openFile($: EngineInterface, path: string): Promise<void> {
  for (const opener of ['open', 'xdg-open']) {
    try {
      if ((await $.process.run([opener, path], { timeoutMs: 5_000 })).exitCode === 0) return
    } catch {
      // not this platform's opener
    }
  }
  $.ui.toast(`zordon: cannot open ${path}`)
}

function workspaceOf(s: ZordonSnapshot): ZordonWorkspace | null {
  return s.kind === 'loading' || s.kind === 'inactive' ? null : s.workspace
}

function failedNames(s: ZordonSnapshot): Set<string> {
  return new Set(s.kind === 'running' ? s.services.filter(x => x.isFailed).map(x => x.name) : [])
}

async function diskUsageKB($: EngineInterface, dir: string): Promise<number | null> {
  try {
    const ran = await $.process.run(['du', '-sk', dir], { timeoutMs: 20_000 })
    const kb = Number(ran.stdout.trim().split(/\s+/)[0])

    return ran.exitCode === 0 && Number.isFinite(kb) ? kb : null
  } catch {
    return null
  }
}

type SizeCache = Map<string, { kb: number | null; at: number }>
type PrCache = Map<string, { pr: Checkout['pr']; at: number }>

const PR_EVERY_MS = 60_000

// The workspace's code as zordon reports it: the services of this level
// whose code is in its src/, grouped by the git tree they build from, each
// tree with its branch and that branch's latest pull request, each service
// with its directory in the tree. A tree inside the workspace reads relative
// to it, a monorepo enclosing it by its name.
export function groupCheckouts(services: ZordonService[], root: string): Omit<Checkout, 'pr'>[] {
  // Only what lives in the workspace's src/: its own checkouts, or in main
  // the project's src/; a service built from elsewhere belongs to Runtime.
  const src = `${root}/src/`
  const inSrc = (p: string | null) => p !== null && p.startsWith(src)
  const byPath = new Map<string, Omit<Checkout, 'pr'>>()
  for (const svc of services) {
    if (svc.isShared || svc.checkout === null) continue
    if (!inSrc(svc.checkout) && !inSrc(svc.sourceDir)) continue
    const path = svc.checkout
    let group = byPath.get(path)
    if (!group) {
      const label = path.startsWith(`${root}/`) ? path.slice(root.length + 1) : (path.split('/').pop() ?? path)
      group = { path, label, branch: svc.branch, apps: [] }
      byPath.set(path, group)
    }
    const dir = svc.sourceDir !== null && svc.sourceDir.startsWith(`${path}/`) ? svc.sourceDir.slice(path.length + 1) : null
    group.apps.push({ name: svc.name, dir })
  }

  return [...byPath.values()].sort((a, b) => a.label.localeCompare(b.label))
}

async function withPullRequests($: EngineInterface, groups: Omit<Checkout, 'pr'>[], prs: PrCache): Promise<Checkout[]> {
  const out: Checkout[] = []
  for (const g of groups) {
    out.push({ ...g, pr: g.branch === null ? null : await pullRequest($, g.path, g.branch, prs) })
  }

  return out
}

// gh answers for the checkout's own remote; without gh, or with no PR for
// the branch, there is none. Asked once a minute per branch.
async function pullRequest($: EngineInterface, dir: string, branch: string, prs: PrCache): Promise<Checkout['pr']> {
  const now = await $.clock.now()
  const key = `${dir}\0${branch}`
  const hit = prs.get(key)
  if (hit && now - hit.at < PR_EVERY_MS) return hit.pr

  let pr: Checkout['pr'] = null
  try {
    const ran = await $.process.run(
      ['gh', 'pr', 'list', '--head', branch, '--state', 'all', '--json', 'url,number', '--limit', '1'],
      { cwd: dir, timeoutMs: 15_000 },
    )
    const found = ran.exitCode === 0 ? (JSON.parse(ran.stdout) as { url: string; number: number }[])[0] : undefined
    if (found && found.url.startsWith('https://')) pr = { url: new URL(found.url).href, number: found.number }
  } catch {
    // no gh, or not a GitHub remote
  }
  prs.set(key, { pr, at: now })

  return pr
}

type VersionCache = {
  binaryVersion: string | null
  binPath: string | null
  resolvedBinPath: string | null
  pluginVersion: string
  binaryCheckedAt: number | null
  latestRelease: { version: string; url: string } | null
  releaseCheckedAt: number | null
}

async function refreshUpdate($: EngineInterface, verCache: VersionCache): Promise<void> {
  const now = await $.clock.now()
  if (verCache.binaryCheckedAt === null || now - verCache.binaryCheckedAt >= VERSION_EVERY_MS) {
    try {
      const verRun = await $.process.run(['zordon', '--version'], { timeoutMs: 5_000 })
      verCache.binaryVersion = verRun.exitCode === 0 ? parseZordonVersion(verRun.stdout) : null
    } catch {
      verCache.binaryVersion = null
    }
    try {
      const whichRun = await $.process.run(['which', 'zordon'], { timeoutMs: 5_000 })
      verCache.binPath = whichRun.exitCode === 0 ? (whichRun.stdout.trim().split('\n')[0] || null) : null
    } catch {
      verCache.binPath = null
    }
    verCache.resolvedBinPath = null
    if (verCache.binPath !== null) {
      try {
        const linkRun = await $.process.run(['readlink', verCache.binPath], { timeoutMs: 2_000 })
        if (linkRun.exitCode === 0) verCache.resolvedBinPath = linkRun.stdout.trim() || null
      } catch {
        verCache.resolvedBinPath = null
      }
    }
    verCache.binaryCheckedAt = now
  }

  const relTtl = verCache.latestRelease !== null ? UPDATE_EVERY_MS : UPDATE_RETRY_MS
  if (verCache.releaseCheckedAt === null || now - verCache.releaseCheckedAt >= relTtl) {
    try {
      const relRun = await $.process.run(
        ['curl', '-fsSL', '--max-time', '3', '-H', 'Accept: application/vnd.github+json', RELEASE_API_URL],
        { timeoutMs: 5_000 },
      )
      if (relRun.exitCode === 0) {
        const parsed = parseLatestRelease(relRun.stdout)
        if (parsed !== null) verCache.latestRelease = parsed
      }
    } catch {
      // curl unavailable or network blocked
    }
    verCache.releaseCheckedAt = now
  }

  const nextUpdate = computeUpdate({
    binaryVersion: verCache.binaryVersion,
    pluginVersion: verCache.pluginVersion,
    latestRelease: verCache.latestRelease,
    binPath: verCache.binPath,
    resolvedBinPath: verCache.resolvedBinPath,
  })
  const prevUpdate = await read($, updateInfo)
  if (JSON.stringify(prevUpdate) !== JSON.stringify(nextUpdate)) {
    await update($, updateInfo, () => nextUpdate)
  }
}

function firstErrorLine(stderr: string, stdout: string, fallback: string): string {
  const raw = stderr.trim() || stdout.trim()
  if (!raw) return fallback

  return raw.split('\n').pop() ?? fallback
}

async function runUpdate($: EngineInterface, verCache: VersionCache): Promise<void> {
  if ((await read($, updateAction)).state === 'running') return
  const upd = await read($, updateInfo)
  if (!upd || !/^\d+\.\d+\.\d+$/.test(upd.latest)) return

  if (upd.isBinaryOutdated) {
    if (upd.binaryMethod === 'brew') {
      await update($, updateAction, () => ({ state: 'running', message: `Upgrading CLI via Homebrew to v${upd.latest}…` }))
      try {
        const brewRun = await $.process.run(['brew', 'upgrade', 'piotrkowalczuk/tap/zordon'], { timeoutMs: 180_000 })
        if (brewRun.exitCode !== 0) {
          const msg = firstErrorLine(brewRun.stderr, brewRun.stdout, 'brew upgrade failed')
          await update($, updateAction, () => ({ state: 'error', message: msg }))
          $.ui.toast(`zordon update failed: ${msg}`)

          return
        }
      } catch (err) {
        const msg = `brew failed: ${String(err)}`
        await update($, updateAction, () => ({ state: 'error', message: msg }))
        $.ui.toast(`zordon update failed: ${msg}`)

        return
      }
    } else {
      await update($, updateAction, () => ({ state: 'running', message: `Downloading zordon v${upd.latest}…` }))
      let platform: { os: 'darwin' | 'linux'; arch: 'arm64' | 'amd64' } | null = null
      try {
        const unameRun = await $.process.run(['uname', '-sm'], { timeoutMs: 5_000 })
        if (unameRun.exitCode === 0) platform = parseUname(unameRun.stdout)
      } catch {
        platform = null
      }
      if (!platform) {
        await update($, updateAction, () => ({ state: 'error', message: 'Unsupported OS or CPU architecture' }))
        $.ui.toast('zordon update failed: unsupported platform')

        return
      }
      try {
        const dlRun = await $.process.run(
          ['sh', '-euc', UPGRADE_TARBALL_SH, 'sh', upd.latest, platform.os, platform.arch, upd.binaryTargetDir],
          { timeoutMs: 120_000 },
        )
        if (dlRun.exitCode !== 0) {
          const msg = firstErrorLine(dlRun.stderr, dlRun.stdout, 'release download failed')
          await update($, updateAction, () => ({ state: 'error', message: msg }))
          $.ui.toast(`zordon update failed: ${msg}`)

          return
        }
      } catch (err) {
        const msg = `download failed: ${String(err)}`
        await update($, updateAction, () => ({ state: 'error', message: msg }))
        $.ui.toast(`zordon update failed: ${msg}`)

        return
      }
    }
    verCache.binaryCheckedAt = null
  }

  if (upd.isPluginOutdated) {
    await update($, updateAction, () => ({ state: 'running', message: `Updating plugin zordon@zordon…` }))
    try {
      const plugRun = await $.process.run(['claude', 'plugin', 'update', 'zordon@zordon'], { timeoutMs: 60_000 })
      if (plugRun.exitCode !== 0) {
        await refreshUpdate($, verCache)
        const msg = 'Run /plugin update zordon@zordon to finish plugin update'
        await update($, updateAction, () => ({ state: 'error', message: msg }))
        $.ui.toast(`zordon: ${msg}`)

        return
      }
      verCache.pluginVersion = upd.latest
    } catch {
      await refreshUpdate($, verCache)
      const msg = 'Run /plugin update zordon@zordon to finish plugin update'
      await update($, updateAction, () => ({ state: 'error', message: msg }))
      $.ui.toast(`zordon: ${msg}`)

      return
    }
  }

  await refreshUpdate($, verCache)
  const doneMsg = upd.isPluginOutdated
    ? `Updated to v${upd.latest} — restart session to reload plugin`
    : `Updated CLI to v${upd.latest}`
  await update($, updateAction, () => ({ state: 'done', message: doneMsg }))
  $.ui.toast(`zordon: ${doneMsg}`)
}

async function refresh($: EngineInterface, sizes: SizeCache, prs: PrCache, verCache: VersionCache): Promise<ZordonSnapshot> {
  let next: ZordonSnapshot
  try {
    // The session's directory now, so a /cd moves what the pane watches.
    const cwd = await $.session.cwd()
    sessionCwd = cwd
    const report = await $.process.run(['zordon', 'status', '--format=json'], { cwd, timeoutMs: 10_000 })
    const parsed = parseAgentStatus(report.stdout)
    if (parsed !== null) {
      next = parsed
    } else {
      const ran = await $.process.run(['zordon', 'status'], { cwd, timeoutMs: 10_000 })
      next = parseStatus(ran.exitCode, ran.stdout, ran.stderr, cwd)
    }
  } catch (err) {
    // Not running zordon is no answer about the tree: an error, never "Not a
    // workspace".
    next = { kind: 'error', message: `cannot run zordon: ${String(err)}`, workspace: null }
  }

  await refreshUpdate($, verCache)

  const ws0 = workspaceOf(next)
  if (ws0 && next.kind !== 'loading' && next.kind !== 'inactive') {
    const dir = ws0.stateDir
    const now = await $.clock.now()
    let cached = sizes.get(dir)
    if (!cached || now - cached.at > SIZE_EVERY_MS) {
      cached = { kb: await diskUsageKB($, dir), at: now }
      sizes.set(dir, cached)
    }
    next = { ...next, workspace: { ...ws0, sizeKB: cached.kb } }

    const services = next.kind === 'running' || next.kind === 'stopped' ? next.services : []
    const root = ws0.name === 'main' ? dirname(ws0.alphasfile) : dir
    const found = await withPullRequests($, groupCheckouts(services, root), prs)
    if (JSON.stringify(found) !== JSON.stringify(await read($, checkouts))) {
      await update($, checkouts, () => found)
    }
  }

  const prev = await read($, snapshot)
  if (JSON.stringify(prev) === JSON.stringify(next)) {
    return next
  }

  const prevFailed = failedNames(prev)
  const newlyFailed = [...failedNames(next)].filter(n => !prevFailed.has(n))
  const isNewError = next.kind === 'error' && prev.kind !== 'error'
  if (isNewError || newlyFailed.length > 0) {
    $.ui.toast(isNewError ? 'zordon failed' : `zordon: ${newlyFailed.join(', ')} failed`)
    await openPane($, false)
  }

  await update($, snapshot, () => next)
  $.ui.status(summary(next))

  return next
}

// Band colors per theme: header and footer a step off the pane's background,
// sub-headers half a step, so both read on a dark and a light terminal.
const PALETTE = {
  dark: { bar: '#2b3040', sub: '#232733' },
  light: { bar: '#dde1ea', sub: '#eceef3' },
} as const

async function palette($: EngineInterface): Promise<(typeof PALETTE)[keyof typeof PALETTE]> {
  try {
    const theme = (await $.config.list()).find(row => row.key === 'theme')?.value

    return typeof theme === 'string' && theme.includes('light') ? PALETTE.light : PALETTE.dark
  } catch {
    return PALETTE.dark
  }
}

// The zordon MCP tools take `{ args: [...] }`; shown as the CLI would read.
function describeArgs(input: Record<string, unknown>): string {
  const args = input.args
  if (Array.isArray(args)) return args.map(String).join(' ')
  const rest = Object.keys(input).length > 0 ? JSON.stringify(input) : ''

  return rest === '{}' ? '' : rest
}

// What a call answered: the text the model read, else the raw result.
function callOutput(ran: object): string {
  const r = ran as { deny?: string; text?: string; result?: unknown }
  if (r.deny !== undefined) return `denied: ${r.deny}`
  if (typeof r.text === 'string') return r.text
  if (r.result === undefined) return ''

  return typeof r.result === 'string' ? r.result : JSON.stringify(r.result, null, 2)
}

function formatDuration(ms: number | null): string {
  if (ms === null) return '…'

  return ms < 1000 ? `${Math.round(ms)}ms` : `${(ms / 1000).toFixed(1)}s`
}

type RenderEvent = Parameters<EngineInterface['ui']['resolve']>[0]

// The pane's column: a Zordon header over three boxes, each folding on its
// own: the workspace and the git checkouts under its src/, every service its run brings up,
// and the zordon MCP calls Claude made this session. With no workspace a
// single box says so instead.
async function stackView($: EngineInterface, e: RenderEvent, s: ZordonSnapshot, verCache: VersionCache) {
  const { Box, Text, Link, Markdown, Button, Code } = $.ui.resolve(e)
  const colors = await palette($)
  const collapsed = await read($, isCollapsed)
  const isStackFolded = await read($, isScopeCollapsed)
  const isRuntimeFolded = await read($, isRuntimeCollapsed)
  const isLogFolded = await read($, isLogCollapsed)
  const log = await read($, calls)
  const opened = await read($, openCall)

  const panel = (p: {
    key: string
    title: string
    aside?: RenderChildren
    border: string
    isFolded: boolean
    onToggle: () => void
    body: RenderChildren
    footer?: { left: RenderChildren; right: RenderChildren }
  }) => (
    <Box key={p.key} flexDirection="column" borderStyle="round" borderColor={p.border} marginTop={1}>
      <Box justifyContent="space-between" backgroundColor={colors.sub} paddingX={1}>
        <Text bold>{p.title}</Text>
        <Box gap={1}>
          {p.aside ?? null}
          <Button key={`fold-${p.key}`} plain label={p.isFolded ? '+' : '−'} onPress={p.onToggle} />
        </Box>
      </Box>
      {p.isFolded ? null : (
        <Box flexDirection="column" paddingX={1}>
          {p.body}
        </Box>
      )}
      {/* Left and right as the row's own two children: handed over as one
          fragment they were laid out as one, one under the other. */}
      {p.footer ? (
        <Box justifyContent="space-between" backgroundColor={colors.sub} paddingX={1}>
          {p.footer.left}
          {p.footer.right}
        </Box>
      ) : null}
    </Box>
  )

  const openedService = await read($, openService)
  const rows = (list: ZordonService[]) => (
    <Box flexDirection="column">
      {list.map(svc => {
        const mark = serviceMark(svc)
        const tags = [mark.note, svc.isShared ? 'shared' : null].filter(Boolean).join(' · ')
        const isOpen = openedService === svc.name
        const details: [string, string | null][] = [
          ['state', svc.state],
          ['print', svc.print.join('\n') || null],
          ['checkout', svc.checkout],
          ['branch', svc.branch],
          ['source', svc.sourceDir],
        ]

        return (
          <Box key={`svc-${svc.name}`} flexDirection="column">
            <Box>
              <Text color={mark.color}>{mark.glyph} </Text>
              <Button
                key={`details-${svc.name}`}
                plain
                label={`${isOpen ? '▾' : '▸'} ${svc.name}`}
                onPress={() => update($, openService, o => (o === svc.name ? null : svc.name))}
              />
              {tags ? <Text dimColor wrap="truncate-end"> {tags}</Text> : null}
            </Box>
            {isOpen ? (
              <Box flexDirection="column" paddingLeft={2}>
                {details
                  .filter((d): d is [string, string] => d[1] !== null)
                  .map(([k, v]) => (
                    <Text key={`detail-${svc.name}-${k}`} wrap="wrap">
                      <Text dimColor>{k} </Text>
                      {v}
                    </Text>
                  ))}
              </Box>
            ) : (
              svc.print.map((line, i) => (
                <Text key={`print-${svc.name}-${String(i)}`} dimColor wrap="truncate-end">
                  {'  '}
                  {line}
                </Text>
              ))
            )}
          </Box>
        )
      })}
    </Box>
  )
  const border = (list: ZordonService[]) =>
    list.some(x => x.isFailed) ? 'red' : list.length > 0 && list.every(x => x.state === 'stopped') ? 'gray' : 'green'

  const header = (
    <Box justifyContent="space-between" backgroundColor={colors.bar} paddingX={1}>
      <Text bold>
        Zordon
        {collapsed && summary(s) ? <Text dimColor> · {summary(s)?.replace(/^zordon /, '')}</Text> : null}
      </Text>
      <Box gap={1}>
        <Link href="https://zordon.io/" label="zordon.io" />
        <Button key="collapse" plain label={collapsed ? '+' : '−'} onPress={() => update($, isCollapsed, c => !c)} />
      </Box>
    </Box>
  )
  if (collapsed) {
    return <Box flexDirection="column">{header}</Box>
  }

  const ws = workspaceOf(s)
  const services = s.kind === 'running' || s.kind === 'stopped' ? s.services : []
  const checkedOut = await read($, checkouts)
  const upd = await read($, updateInfo)
  const dismissed = await read($, dismissedUpdate)
  const act = await read($, updateAction)
  const shownUpdate = upd !== null && dismissed !== upd.latest ? upd : null
  const updateBox = shownUpdate ? (
    <Box key="update" flexDirection="column" borderStyle="round" borderColor="yellow" marginTop={1}>
      <Box justifyContent="space-between" backgroundColor={colors.sub} paddingX={1}>
        <Text bold color="yellow">
          Update · v{shownUpdate.latest}
        </Text>
        <Box gap={1}>
          <Button
            key="run-update-pane"
            label={act.state === 'running' ? 'Updating…' : 'Update'}
            variant="primary"
            onPress={() => runUpdate($, verCache)}
          />
          <Link href={shownUpdate.releaseUrl} label="release" />
          <Button
            key="dismiss-update-pane"
            plain
            label="×"
            onPress={() => update($, dismissedUpdate, () => shownUpdate.latest)}
          />
        </Box>
      </Box>
      <Box flexDirection="column" paddingX={1}>
        {shownUpdate.isBinaryOutdated ? (
          <Text wrap="wrap">
            <Text dimColor>cli v{shownUpdate.binaryVersion} → v{shownUpdate.latest}: </Text>
            {shownUpdate.binaryUpgradeCmd}
          </Text>
        ) : null}
        {shownUpdate.isPluginOutdated ? (
          <Text wrap="wrap">
            <Text dimColor>plugin {shownUpdate.pluginVersion} → {shownUpdate.latest}: </Text>
            {shownUpdate.pluginUpgradeCmd}
          </Text>
        ) : null}
        {act.message ? (
          <Text color={act.state === 'error' ? 'red' : act.state === 'done' ? 'green' : 'yellow'} wrap="wrap">
            {act.message}
          </Text>
        ) : null}
      </Box>
    </Box>
  ) : null

  // No workspace: one box saying so, in place of Workspace, Runtime and Logs.
  // Before zordon first answered: no verdict on the tree yet.
  if (s.kind === 'loading') {
    return (
      <Box flexDirection="column">
        {header}
        {updateBox}
        <Box borderStyle="round" borderColor="gray" marginTop={1} paddingX={1}>
          <Text dimColor>Reading the stack…</Text>
        </Box>
      </Box>
    )
  }

  if (s.kind === 'inactive') {
    return (
      <Box flexDirection="column">
        {header}
        {updateBox}
        <Box flexDirection="column" borderStyle="round" borderColor="gray" marginTop={1} paddingX={1}>
          <Text>
            <Text color="gray">○ </Text>
            <Text bold>Not a workspace</Text>
          </Text>
          <Text dimColor wrap="wrap">
            No Alphasfile anywhere in this tree
          </Text>
          <Text dimColor>
            Learn more at <Link href="https://zordon.io/" label="zordon.io" />
          </Text>
        </Box>
      </Box>
    )
  }

  const checkoutRows = (
    <Box flexDirection="column">
      {checkedOut.map(c => (
        <Box key={`co-${c.path}`} flexDirection="column">
          <Box gap={1}>
            <Text bold wrap="truncate-end">
              {c.label}
            </Text>
            {c.pr ? (
              <Link href={c.pr.url} label={c.branch ?? ''} />
            ) : (
              <Text dimColor wrap="truncate-end">
                {c.branch ?? 'detached'}
              </Text>
            )}
            {c.pr ? <Text dimColor>#{c.pr.number}</Text> : null}
          </Box>
          {c.apps.map(a => (
            <Text key={`app-${c.path}-${a.name}`} wrap="truncate-end">
              {'  '}
              {a.name}
              {a.dir ? <Text dimColor> {a.dir}</Text> : null}
            </Text>
          ))}
        </Box>
      ))}
    </Box>
  )
  const stackBody =
    s.kind === 'error' ? (
      <Markdown text={['❌ **zordon error**', '', '```', s.message, '```'].join('\n')} />
    ) : checkedOut.length > 0 ? (
      checkoutRows
    ) : (
      <Text dimColor>
        {ws ? `nothing in ${ws.name === 'main' ? dirname(ws.alphasfile) : ws.stateDir}/src` : 'no src/'}
      </Text>
    )

  const callRows = (
    <Box flexDirection="column">
      {log.map(c => {
        const isOpen = c.id === opened
        const mark = c.durationMs === null ? '◐' : c.isError ? '✕' : '✓'
        const color = c.durationMs === null ? 'yellow' : c.isError ? 'red' : 'green'

        return (
          <Box key={`call-${c.id}`} flexDirection="column">
            <Box>
              <Text color={color}>{mark} </Text>
              <Button
                key={`open-${c.id}`}
                plain
                label={`${c.command}${c.args ? ` ${c.args}` : ''}`}
                onPress={() => update($, openCall, o => (o === c.id ? null : c.id))}
              />
              <Text dimColor> {formatDuration(c.durationMs)}</Text>
            </Box>
            {isOpen ? (
              <Box flexDirection="column" paddingLeft={2}>
                <Text dimColor>
                  {new Date(c.startedAt).toLocaleTimeString()} · zordon {c.command}
                  {c.args ? ` ${c.args}` : ''}
                </Text>
                {c.output ? (
                  <Code
                    source={c.output}
                    language={c.output.trimStart().startsWith('{') ? 'json' : 'text'}
                    wrap="truncate-end"
                  />
                ) : (
                  <Text dimColor>no output yet</Text>
                )}
              </Box>
            ) : null}
          </Box>
        )
      })}
    </Box>
  )

  return (
    <Box flexDirection="column">
      {header}
      {updateBox}
      {panel({
        key: 'workspace',
        title: 'Workspace',
        aside: ws ? <Text dimColor>{ws.name}</Text> : null,
        border: s.kind === 'error' ? 'red' : checkedOut.length > 0 ? 'green' : 'gray',
        isFolded: isStackFolded,
        onToggle: () => update($, isScopeCollapsed, f => !f),
        body: stackBody,
        footer: ws
          ? {
              left: <Text dimColor>{ws.sizeKB != null ? formatSize(ws.sizeKB) : '…'}</Text>,
              right: <Button key="open-alphasfile" plain label="Alphasfile" onPress={() => openFile($, ws.alphasfile)} />,
            }
          : undefined,
      })}
      {services.length === 0
        ? null
        : panel({
            key: 'runtime',
            title: 'Runtime',
            border: border(services),
            isFolded: isRuntimeFolded,
            onToggle: () => update($, isRuntimeCollapsed, f => !f),
            body: rows([...services].sort(byStateThenName)),
          })}
      {log.length === 0 || ws === null
        ? null
        : panel({
            key: 'log',
            title: `Logs · ${log.length}`,
            border: 'gray',
            isFolded: isLogFolded,
            onToggle: () => update($, isLogCollapsed, f => !f),
            body: callRows,
          })}
    </Box>
  )
}

export const register: Register = on => {
  const sizes: SizeCache = new Map()
  const prs: PrCache = new Map()
  const verCache: VersionCache = {
    binaryVersion: null,
    binPath: null,
    resolvedBinPath: null,
    pluginVersion: PLUGIN_VERSION,
    binaryCheckedAt: null,
    latestRelease: null,
    releaseCheckedAt: null,
  }

  registerDiff(on)

  on('tool.call', async ($, e, next) => {
    const command = MCP_TOOL.exec(e.tool)?.[1]
    if (command === undefined) return next(e)

    const { tool, tool_use_id: toolUseId, consent: _consent, ...input } = e as Record<string, unknown> & {
      tool: string
      tool_use_id?: string
      consent?: string
    }
    const startedAt = await $.clock.now()
    const call: McpCall = {
      id: toolUseId ?? `${tool}-${startedAt}`,
      command,
      args: describeArgs(input),
      startedAt,
      durationMs: null,
      isError: false,
      output: '',
    }
    await update($, calls, list => [call, ...list].slice(0, MAX_CALLS))

    const ran = await next(e)
    const durationMs = (await $.clock.now()) - startedAt
    const output = callOutput(ran)
    const isError = ('deny' in ran && ran.deny !== undefined) || ('isError' in ran && ran.isError === true)
    await update($, calls, list =>
      list.map(c => (c.id === call.id ? { ...c, durationMs, isError, output: output.slice(0, MAX_OUTPUT) } : c)),
    )

    return ran
  })

  on('session.start', async ($, e, next) => {
    const started = await next(e)
    sessionCwd = e.cwd

    let isPolling = false
    const tick = async () => {
      if (isPolling) return
      isPolling = true
      try {
        await refresh($, sizes, prs, verCache)
      } finally {
        isPolling = false
      }
    }
    void tick().then(async () => {
      if (workspaceOf(await current($)) !== null) await openPane($, false)
    })
    $.clock.every(POLL_MS, tick)

    return started
  })

  // /zordon:dashboard is the plugin's commands/dashboard.md; answering it here
  // toggles the pane instead of sending that file's fallback prompt.
  on('command.run', { command: DASHBOARD }, async $ => {
    // Opened unasked on a narrow terminal the pane is open but not shown: the
    // person asking for it wants it seated, not closed.
    if ((await $.ui.panes()).some(p => p.id === PANE && p.isShown)) {
      await $.ui.close({ id: PANE })

      return { text: 'zordon dashboard hidden' }
    }

    await refresh($, sizes, prs, verCache)
    const s = await current($)
    const opened = await openPane($, true)
    const where = opened.isPlaced ? 'shown' : `waits: ${opened.reason}`

    return { text: `zordon dashboard ${where}: ${summary(s) ?? `no Alphasfile at or above ${sessionCwd}`}` }
  })

  // The same view inline, as the /zordon:dashboard row in the transcript: what a
  // surface that seats no panes (a phone over Remote Control) still shows.
  on('ui.render', { component: 'CommandOutput', props: { command: DASHBOARD } }, async ($, e, next) => {
    if (e.props.isErrored || e.props.text.endsWith('hidden')) return next(e)

    return stackView($, e, await current($), verCache)
  })

  // The stack's view: a pane, docked beside the transcript in fullscreen
  // (inline above the prompt otherwise); the arrows scroll it once focused.
  on('ui.render', { component: 'Pane', requestId: PANE }, async ($, e) => stackView($, e, await current($), verCache))

  // Above the prompt: errors first, otherwise an update notice in active workspaces.
  on('ui.render', { component: 'AbovePrompt' }, async ($, e, next) => {
    if (e.props.hasSurvey) {
      return next(e)
    }

    const s = await current($)
    const isBad = s.kind === 'error' || (s.kind === 'running' && s.services.some(x => x.isFailed))
    if (isBad) {
      const { Box, Button, Text } = $.ui.resolve(e)

      return (
        <Box>
          <Text color="red">{summary(s)} </Text>
          <Button key="open" label="Show" onPress={() => openPane($, true)} />
        </Box>
      )
    }

    const upd = await read($, updateInfo)
    const dismissed = await read($, dismissedUpdate)
    const act = await read($, updateAction)
    if (s.kind === 'loading' || s.kind === 'inactive' || upd === null || dismissed === upd.latest) {
      return next(e)
    }

    const { Box, Button, Text } = $.ui.resolve(e)

    return (
      <Box gap={1}>
        <Text color={act.state === 'error' ? 'red' : 'yellow'}>
          {act.state === 'running' && act.message ? act.message : updatePromptLine(upd)}
        </Text>
        <Button
          key="run-update"
          label={act.state === 'running' ? 'Updating…' : 'Update'}
          onPress={() => runUpdate($, verCache)}
        />
        <Button key="open-update" plain label="Show" onPress={() => openPane($, true)} />
        <Button key="dismiss-update" plain label="Dismiss" onPress={() => update($, dismissedUpdate, () => upd.latest)} />
      </Box>
    )
  })
}
