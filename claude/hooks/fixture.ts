import { mock } from 'claude-code/testing'
import type { Engine } from 'claude-code/testing'
import type { On } from 'claude-code'

// A running `feature` workspace as `zordon status --format=json` reports it.
export const FEATURE = JSON.stringify({
  workspace: 'feature',
  alphasfile: '/proj/Alphasfile',
  state_dir: '/proj/workspaces/feature',
  state: 'running',
  services: [
    {
      name: 'api',
      state: 'ready',
      picked: true,
      print: 'http://localhost:8080',
      checkout_path: '/proj/workspaces/feature/src/api',
      branch: 'zordon/feature/api',
      source_dir: '/proj/workspaces/feature/src/api/cmd/api',
    },
    {
      name: 'worker',
      state: 'probing',
      picked: true,
      checkout_path: '/proj/workspaces/feature/src/worker',
      branch: 'zordon/feature/worker',
    },
    {
      name: 'gateway',
      state: 'ready',
      print: 'http://localhost:8000',
      checkout_path: '/proj',
      branch: 'main',
      source_dir: '/proj/gateway',
    },
    { name: 'postgres', state: 'ready', shared: true },
    { name: 'kafka', state: 'ready', shared: true },
  ],
})

// The feature workspace's src/: api and worker checked out as git trees.
const TREE: Record<string, string[]> = {
  '/proj/workspaces/feature/src': ['/proj/workspaces/feature/src/api', '/proj/workspaces/feature/src/worker'],
}

export const PR = { url: 'https://github.com/acme/api/pull/7', number: 7 }

export const PANE = {
  component: 'Pane',
  requestId: 'zordon',
  props: {
    title: 'zordon',
    isFocused: false,
    bodyColumns: 56,
    placement: 'dock',
    scroll: { offset: 0, bodyRows: 40 },
    view: {},
  },
} as const

// Stands for the host beneath the plugin: zordon answering `status` with
// `stack`, du sizing the workspace, a surface seating the pane. Register it
// before the test first calls $.
export function host(on: On, stack: string, pane: { isShown: boolean } = { isShown: true }) {
  const clock = mock.clock(on, { now: 1_000 })
  const done = (stdout: string, exitCode = 0) => ({
    value: { exitCode, stdout, stderr: '', isStdoutTruncated: false, isStderrTruncated: false },
  })
  on('fs.exists', ($, e) => ({ value: e.path in TREE || Object.values(TREE).some(dirs => dirs.some(d => `${d}/.git` === e.path)) }))
  on('fs.list', ($, e) => ({ value: (TREE[e.path] ?? []).map(d => ({ name: d.split('/').pop() ?? d, kind: 'dir' as const, size: 0, mtimeMs: 0, isLink: false })) }))
  on('process.run', ($, e) => {
    if (e.argv[0] === 'git') return done(`zordon/feature/${e.argv[2]?.split('/').pop()}\n`)
    if (e.argv[0] === 'gh') return done(e.init?.cwd?.endsWith('/api') ? JSON.stringify([PR]) : '[]')
    if (e.argv[0] === 'zordon') return { value: { exitCode: 0, stdout: `${stack}\n`, stderr: '', isStdoutTruncated: false, isStderrTruncated: false } }
    if (e.argv[0] === 'du') return { value: { exitCode: 0, stdout: `1363148\t${e.argv[2]}\n`, stderr: '', isStdoutTruncated: false, isStderrTruncated: false } }

    return { value: { exitCode: 127, stdout: '', stderr: `${e.argv[0]}: not found`, isStdoutTruncated: false, isStderrTruncated: false } }
  })
  on('session.cwd', () => ({ value: '/proj/workspaces/feature' }))
  const open = new Set<string>()
  on('ui.open', ($, e) => {
    open.add(e.id)

    return { value: { isPlaced: true } }
  })
  on('ui.close', ($, e) => {
    open.delete(e.id)

    return { value: undefined }
  })
  on('ui.panes', () => ({
    value: [...open].map(id => ({ id, title: id, isShown: pane.isShown, isFocused: false, isPlaced: pane.isShown, plugin: 'zordon' })),
  }))

  return clock
}

// /zordon:dashboard as a person types it: shows the pane, or hides an open one.
export async function openStack($: Engine): Promise<string | undefined> {
  const ran = await $.command.run({
    command: 'zordon:dashboard',
    args: '',
    origin: { kind: 'composer' },
    presentation: { isFullscreen: true, columns: 160 },
  })

  return ran.text
}
