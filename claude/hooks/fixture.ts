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
    { name: 'api', state: 'ready', picked: true, print: 'http://localhost:8080' },
    { name: 'worker', state: 'probing', picked: true },
    { name: 'gateway', state: 'ready', print: 'http://localhost:8000' },
    { name: 'postgres', state: 'ready', shared: true },
    { name: 'kafka', state: 'ready', shared: true },
  ],
})

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
export function host(on: On, stack: string) {
  const clock = mock.clock(on, { now: 1_000 })
  on('process.run', ($, e) => {
    if (e.argv[0] === 'zordon') return { value: { exitCode: 0, stdout: `${stack}\n`, stderr: '', isStdoutTruncated: false, isStderrTruncated: false } }
    if (e.argv[0] === 'du') return { value: { exitCode: 0, stdout: `1363148\t${e.argv[2]}\n`, stderr: '', isStdoutTruncated: false, isStderrTruncated: false } }

    return { value: { exitCode: 127, stdout: '', stderr: `${e.argv[0]}: not found`, isStdoutTruncated: false, isStderrTruncated: false } }
  })
  on('ui.open', () => ({ value: { isPlaced: true } }))

  return clock
}

// /zordon as a person types it: refreshes the stack and opens the pane.
export async function openStack($: Engine): Promise<void> {
  await $.command.run({
    command: 'zordon',
    args: '',
    origin: { kind: 'composer' },
    presentation: { isFullscreen: true, columns: 160 },
  })
}
