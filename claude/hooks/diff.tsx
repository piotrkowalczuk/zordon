import { atom, read, update } from 'claude-code'
import type { EngineInterface, On } from 'claude-code'

import type { DiffApp, DiffFile, DiffView } from '../types'

const PANE = 'zordon-diff'
const SRC = 'src'
const CODE_LIMIT = 10_000
const MAX_FILES = 40

const view = atom(
  { plugin: 'zordon', key: 'diff' } as const,
  { apps: [], app: null, files: [], more: 0, isLoading: false } as DiffView,
)

const FILE_HEADER = /^diff --git a\/.+? b\/(.+)$/

// Cuts a file's hunks to what one Code element takes, at a hunk boundary so
// what is left still parses as a diff.
function fitHunks(hunks: string): string | null {
  if (hunks.length <= CODE_LIMIT) return hunks
  const cut = hunks.lastIndexOf('\n@@', CODE_LIMIT)

  return cut > 0 ? hunks.slice(0, cut) : null
}

export function parseGitDiff(out: string): DiffFile[] {
  const files: DiffFile[] = []
  let current: { path: string; lines: string[] } | null = null

  const flush = () => {
    if (current === null) return
    const start = current.lines.findIndex(l => l.startsWith('@@'))
    const body = start < 0 ? [] : current.lines.slice(start)
    const added = body.filter(l => l.startsWith('+')).length
    const removed = body.filter(l => l.startsWith('-')).length
    const isBinary = current.lines.some(l => l.startsWith('Binary files'))
    const hunks = body.length > 0 ? fitHunks(body.join('\n').trimEnd()) : null
    const note = isBinary
      ? 'binary file'
      : body.length === 0
        ? 'mode or rename only'
        : hunks === null
          ? 'diff too large to show'
          : null
    files.push({ path: current.path, added, removed, hunks, note })
  }

  for (const line of out.split('\n')) {
    const header = FILE_HEADER.exec(line)
    if (header) {
      flush()
      current = { path: header[1] ?? '?', lines: [] }
    } else if (current !== null) {
      current.lines.push(line)
    }
  }
  flush()

  return files
}

async function git($: EngineInterface, app: string, args: string[]): Promise<string> {
  const ran = await $.process.run(['git', '-C', `${SRC}/${app}`, ...args], { timeoutMs: 20_000 })

  return ran.exitCode === 0 ? ran.stdout : ''
}

async function discoverApps($: EngineInterface): Promise<DiffApp[]> {
  if (!(await $.fs.exists(SRC))) return []
  const dirs = (await $.fs.list(SRC)).filter(d => d.kind === 'dir').map(d => d.name)
  const apps: DiffApp[] = []
  for (const name of dirs.sort()) {
    if (!(await $.fs.exists(`${SRC}/${name}/.git`))) continue
    const status = await git($, name, ['status', '--porcelain'])
    apps.push({ name, changes: status.split('\n').filter(Boolean).length })
  }

  return apps
}

async function loadApp($: EngineInterface, app: string): Promise<void> {
  await update($, view, v => ({ ...v, app, isLoading: true }))

  const tracked = parseGitDiff(await git($, app, ['diff', 'HEAD', '--no-color', '--no-ext-diff']))
  const untracked = (await git($, app, ['ls-files', '--others', '--exclude-standard']))
    .split('\n')
    .filter(Boolean)
    .map((path): DiffFile => ({ path, added: 0, removed: 0, hunks: null, note: 'untracked' }))
  const all = [...tracked, ...untracked]

  await update($, view, v =>
    v.app === app
      ? { ...v, files: all.slice(0, MAX_FILES), more: Math.max(0, all.length - MAX_FILES), isLoading: false }
      : v,
  )
}

export function registerDiff(on: On): void {
  on('command.run', { command: 'diff' }, async ($, e, next) => {
    const apps = await discoverApps($)
    if (apps.length === 0) {
      return next(e)
    }

    const asked = e.args.trim()
    const app = asked === '' ? (apps.find(a => a.changes > 0) ?? apps[0])?.name : asked
    if (app === undefined || !apps.some(a => a.name === app)) {
      return { text: `No app "${asked}" under ${SRC}/. Known: ${apps.map(a => a.name).join(', ')}` }
    }

    await update($, view, v => ({ ...v, apps }))
    await loadApp($, app)
    await $.ui.open({ id: PANE, title: `diff · ${SRC}/${app}`, focus: true })
    const files = (await read($, view)).files.length

    return { text: `diff of ${SRC}/${app}: ${files} file(s) changed` }
  })

  on('ui.render', { component: 'Pane', requestId: PANE }, async ($, e) => {
    const v = await read($, view)
    const ui = $.ui.resolve(e)
    const { Box, Text, Button, Code } = ui

    const pick = async (app: string) => {
      await loadApp($, app)
      await $.ui.open({ id: PANE, title: `diff · ${SRC}/${app}` })
    }
    const options = v.apps.map(a => ({ value: a.name, label: `${a.name} · ${a.changes}` }))

    // The phone has no Select: one Button per app there.
    const picker =
      'Select' in ui ? (
        <ui.Select key="app" label="app " options={options} value={v.app ?? undefined} autoFocus onSelect={pick} />
      ) : (
        <Box flexWrap="wrap">
          {v.apps.map(a => (
            <Button
              key={`app-${a.name}`}
              label={`${a.name} · ${a.changes}`}
              variant={a.name === v.app ? 'primary' : undefined}
              onPress={() => pick(a.name)}
            />
          ))}
        </Box>
      )

    return (
      <Box flexDirection="column">
        {v.apps.length > 1 ? picker : null}
        {v.isLoading ? <Text dimColor>loading…</Text> : null}
        {!v.isLoading && v.files.length === 0 ? <Text dimColor>No changes in {SRC}/{v.app}.</Text> : null}
        {v.files.map(f => (
          <Box key={`f-${f.path}`} flexDirection="column" marginTop={1}>
            <Text bold>
              {f.path} <Text color="green">+{f.added}</Text> <Text color="red">−{f.removed}</Text>
            </Text>
            {f.hunks !== null ? <Code source={f.hunks} format="diff" path={f.path} /> : null}
            {f.note !== null ? <Text dimColor>{f.note}</Text> : null}
          </Box>
        ))}
        {v.more > 0 ? <Text dimColor>… {v.more} more file(s)</Text> : null}
      </Box>
    )
  })
}
