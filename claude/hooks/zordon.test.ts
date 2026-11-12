import { expect, test } from 'claude-code/testing'

import {
  binaryUpgradePlan,
  byStateThenName,
  compareSemver,
  computeUpdate,
  formatSize,
  groupCheckouts,
  isBrewBinary,
  parseLatestRelease,
  parseSemver,
  parseStatus,
  parseUname,
  parseZordonVersion,
  splitScope,
  updatePromptLine,
} from './register'

const RUNNING = `# [3aec45fbd85ea76f] /x/Alphasfile (invocation, workspace=main)
  alpha pid=4242 started=2026-10-04T17:00:00Z
  services (3):
    - [go] api — running pid=10 [ready]
        http://localhost:8080
        checkout: /x (branch main)
    - [go] worker — running pid=11 [probing]
    - [pkg] postgres — failed
`

test('counts services that are up and keeps their print lines', () => {
  const s = parseStatus(0, RUNNING, '')
  expect(s.kind).toBe('running')
  if (s.kind !== 'running') return
  expect(s.services.map(x => [x.name, x.isUp, x.isFailed])).toEqual([
    ['api', true, false],
    ['worker', false, false],
    ['postgres', false, true],
  ])
  expect(s.services[0]?.print).toEqual(['http://localhost:8080'])
})

test('a stack with alpha pid=0 is stopped', () => {
  const out = RUNNING.replace('pid=4242', 'pid=0')
  expect(parseStatus(0, out, '').kind).toBe('stopped')
})

test('no Alphasfile means inactive', () => {
  const err = '[2026-10-04T17:56:37+02:00] zordon          [ERROR] - invocation: no Alphasfile at or above /x'
  expect(parseStatus(1, '', err).kind).toBe('inactive')
})

test('a zordon failure surfaces its ERROR lines', () => {
  const err = '[2026-10-04T17:56:37+02:00] zordon          [ERROR] - alpha reported error: port 5432 in use'
  expect(parseStatus(1, '', err)).toEqual({
    kind: 'error',
    message: 'alpha reported error: port 5432 in use',
    workspace: null,
  })
})

test('the invocation header gives the workspace and its state dir', () => {
  const s = parseStatus(0, RUNNING, '')
  expect(s.kind !== 'inactive' && s.kind !== 'loading' && s.workspace).toEqual({
    name: 'main',
    alphasfile: '/x/Alphasfile',
    stateDir: '/x/workspaces/main',
    sizeKB: null,
  })
})

test('sizes read in binary units', () => {
  expect(formatSize(512)).toBe('512 KB')
  expect(formatSize(1_363_148)).toBe('1.3 GB')
})

const FEDERATED = `# [aaaa1111aaaa1111] /infra/Alphasfile
  alpha pid=900 started=2026-10-04T16:00:00Z
  services (1):
    - [pkg] postgres — running pid=901 [ready]
        postgres://localhost:5432

# [bbbb2222bbbb2222] /proj/Alphasfile (invocation, workspace=feature)
  alpha pid=1000 started=2026-10-04T17:00:00Z
  services (2):
    - [go] api — running pid=1001 [ready]
        checkout: /proj/workspaces/feature/src/api (branch zordon/feature/api)
    - [go] gateway — running pid=1002 [ready]
        checkout: /proj (branch main)
`

test('scope is what the workspace picked, runtime the rest and the shared levels', () => {
  const s = parseStatus(0, FEDERATED, '')
  expect(s.kind).toBe('running')
  if (s.kind !== 'running') return
  const { scope, runtime } = splitScope(s.services)
  expect(scope.map(x => x.name)).toEqual(['api'])
  expect(runtime.map(x => [x.name, x.isShared])).toEqual([
    ['postgres', true],
    ['gateway', false],
  ])
  expect(s.workspace?.name).toBe('feature')
})

test('a stopped leaf is stopped even while a shared parent runs', () => {
  const s = parseStatus(0, FEDERATED.replace('alpha pid=1000', 'alpha pid=0'), '')
  expect(s.kind).toBe('stopped')
})

test('runtime sorts failed, then coming up, then running, then stopped, by name within', () => {
  const s = parseStatus(0, `# [aaaa1111aaaa1111] /p/Alphasfile (invocation, workspace=main)
  alpha pid=1 started=x
  services (5):
    - [go] web — stopped
    - [go] db — running pid=3 [ready]
    - [go] api — running pid=2 [ready]
    - [go] queue — running pid=4 [probing]
    - [go] cache — failed
`, '')
  if (s.kind !== 'running') throw new Error(s.kind)
  expect([...s.services].sort(byStateThenName).map(x => x.name)).toEqual(['cache', 'queue', 'api', 'db', 'web'])
})

test('a text report\'s checkout lines give each service its tree and branch, relative ones resolved', () => {
  const s = parseStatus(
    0,
    `# [aaaa1111aaaa1111] /p/Alphasfile (invocation, workspace=demo)
  alpha pid=1 started=x
  services (2):
    - [go] serviceA — running pid=2 [ready]
        checkout: src/serviceA (branch zordon/demo/serviceA)
    - [go] serviceB — running pid=3 [ready]
        checkout: /p (branch main ⚠ not zordon/demo/serviceB — building your branch)
`,
    '',
    '/p/workspaces/demo',
  )
  if (s.kind !== 'running') throw new Error(s.kind)
  // serviceB builds from the project root, outside the workspace's src/
  expect(groupCheckouts(s.services, '/p/workspaces/demo')).toEqual([
    { path: '/p/workspaces/demo/src/serviceA', label: 'src/serviceA', branch: 'zordon/demo/serviceA', apps: [{ name: 'serviceA', dir: null }] },
  ])
})

test('in main, services whose code is in the project\'s src/ show under their monorepo', () => {
  const svc = (name: string, sourceDir: string) => ({
    name, toolchain: 'go', state: 'stopped', isUp: false, isFailed: false, print: [],
    checkout: '/repo', branch: 'main', sourceDir, isShared: false, isPicked: false,
  })
  expect(groupCheckouts([svc('a', '/repo/proj/src/a'), svc('tool', '/repo/tools/x')], '/repo/proj')).toEqual([
    { path: '/repo', label: 'repo', branch: 'main', apps: [{ name: 'a', dir: 'proj/src/a' }] },
  ])
})

test('parses zordon --version lines and ignores unrelated output', () => {
  expect(parseZordonVersion('zordon v0.29.0 (9b4437bd1c2e, 2026-07-23T20:00:00Z, go1.26.5)\n')).toBe('0.29.0')
  expect(parseZordonVersion('zordon v0.29.1-0.20261112184434-cbc6427e35b4\n')).toBe('0.29.1-0.20261112184434-cbc6427e35b4')
  expect(parseZordonVersion('zordon (devel) (7c826464877e, go1.26.5)\n')).toBe('(devel)')
  expect(parseZordonVersion('{"workspace":"feature","state":"running"}')).toBeNull()
})

test('compares semver including prereleases and Go pseudo-versions', () => {
  const a = parseSemver('0.29.0')
  const b = parseSemver('v0.29.1-0.20261112184434-cbc6427e35b4')
  const c = parseSemver('v0.29.1')
  if (!a || !b || !c) throw new Error('expected valid semver')
  expect(compareSemver(a, b)).toBeLessThan(0)
  expect(compareSemver(b, c)).toBeLessThan(0)
  expect(compareSemver(c, c)).toBe(0)
})

test('parses GitHub latest release JSON and rejects drafts or prereleases', () => {
  expect(
    parseLatestRelease(
      JSON.stringify({
        tag_name: 'v0.30.0',
        html_url: 'https://github.com/piotrkowalczuk/zordon/releases/tag/v0.30.0',
        draft: false,
        prerelease: false,
      }),
    ),
  ).toEqual({
    version: '0.30.0',
    url: 'https://github.com/piotrkowalczuk/zordon/releases/tag/v0.30.0',
  })
  expect(parseLatestRelease(JSON.stringify({ tag_name: 'v0.30.0-rc.1', prerelease: true }))).toBeNull()
  expect(parseLatestRelease('not json')).toBeNull()
})

test('computes update targets across CLI, plugin, brew vs tarball download, offline skew, and devel builds', () => {
  const rel30 = { version: '0.30.0', url: 'https://github.com/piotrkowalczuk/zordon/releases/tag/v0.30.0' }

  expect(computeUpdate({ binaryVersion: '0.30.0', pluginVersion: '0.30.0', latestRelease: rel30 })).toBeNull()

  const both = computeUpdate({ binaryVersion: '0.29.0', pluginVersion: '0.29.0', latestRelease: rel30 })
  expect(both?.isBinaryOutdated).toBe(true)
  expect(both?.isPluginOutdated).toBe(true)
  expect(both?.binaryMethod).toBe('download')
  expect(both?.pluginUpgradeCmd).toBe('claude plugin update zordon@zordon')
  expect(both && updatePromptLine(both)).toBe('↑ zordon v0.30.0 available (cli v0.29.0, plugin 0.29.0)')

  const cliDownload = computeUpdate({
    binaryVersion: '0.29.0',
    pluginVersion: '0.30.0',
    latestRelease: rel30,
    binPath: '/Users/dev/go/bin/zordon',
  })
  expect(cliDownload?.isBinaryOutdated).toBe(true)
  expect(cliDownload?.isPluginOutdated).toBe(false)
  expect(cliDownload?.binaryMethod).toBe('download')
  expect(cliDownload?.binaryTargetDir).toBe('/Users/dev/go/bin')
  expect(cliDownload?.binaryUpgradeCmd).toBe('download release → /Users/dev/go/bin')

  const cliBrew = computeUpdate({
    binaryVersion: '0.29.0',
    pluginVersion: '0.30.0',
    latestRelease: rel30,
    binPath: '/usr/local/bin/zordon',
    resolvedBinPath: '../Caskroom/zordon/0.29.0/zordon',
  })
  expect(cliBrew?.binaryMethod).toBe('brew')
  expect(cliBrew?.binaryUpgradeCmd).toBe('brew upgrade piotrkowalczuk/tap/zordon')

  const pluginOffline = computeUpdate({ binaryVersion: '0.30.0', pluginVersion: '0.29.0', latestRelease: null })
  expect(pluginOffline?.isBinaryOutdated).toBe(false)
  expect(pluginOffline?.isPluginOutdated).toBe(true)
  expect(pluginOffline?.latest).toBe('0.30.0')

  expect(computeUpdate({ binaryVersion: '(devel)', pluginVersion: '0.30.0', latestRelease: rel30 })).toBeNull()
  const develPluginBehind = computeUpdate({ binaryVersion: '(devel)', pluginVersion: '0.29.0', latestRelease: rel30 })
  expect(develPluginBehind?.isBinaryOutdated).toBe(false)
  expect(develPluginBehind?.isPluginOutdated).toBe(true)

  expect(isBrewBinary('/opt/homebrew/bin/zordon')).toBe(true)
  expect(isBrewBinary('/usr/local/bin/zordon')).toBe(false)
  expect(isBrewBinary('/usr/local/bin/zordon', '/usr/local/Caskroom/zordon/0.29.0/zordon')).toBe(true)
  expect(binaryUpgradePlan(null)).toEqual({
    method: 'download',
    targetDir: '',
    label: 'download release → ~/.local/bin',
  })

  expect(parseUname('Darwin arm64\n')).toEqual({ os: 'darwin', arch: 'arm64' })
  expect(parseUname('Darwin x86_64\n')).toEqual({ os: 'darwin', arch: 'amd64' })
  expect(parseUname('Linux aarch64\n')).toEqual({ os: 'linux', arch: 'arm64' })
  expect(parseUname('Linux x86_64\n')).toEqual({ os: 'linux', arch: 'amd64' })
  expect(parseUname('MINGW64_NT x86_64\n')).toBeNull()
})
