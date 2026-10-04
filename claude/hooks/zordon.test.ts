import { expect, test } from 'claude-code/testing'

import { formatSize, parseStatus, splitScope } from './register'

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
  expect(s.kind !== 'inactive' && s.workspace).toEqual({
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
