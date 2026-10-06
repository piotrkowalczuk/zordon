import { atom, read, update } from 'claude-code'
import type { EngineInterface, Register, RenderChildren, UiOpenResult } from 'claude-code'

import { registerDiff } from './diff'
import type { McpCall, ZordonService, ZordonSnapshot, ZordonWorkspace } from '../types'

const POLL_MS = 5000
const SIZE_EVERY_MS = 60_000

const snapshot = atom(
  { plugin: 'zordon', key: 'snapshot' } as const,
  { kind: 'inactive' } as ZordonSnapshot,
)
const isCollapsed = atom({ plugin: 'zordon', key: 'isCollapsed' } as const, false)
const isScopeCollapsed = atom({ plugin: 'zordon', key: 'isScopeCollapsed' } as const, false)
const isRuntimeCollapsed = atom({ plugin: 'zordon', key: 'isRuntimeCollapsed' } as const, false)
const isLogCollapsed = atom({ plugin: 'zordon', key: 'isLogCollapsed' } as const, false)
const calls = atom({ plugin: 'zordon', key: 'calls' } as const, [] as McpCall[])
const openCall = atom({ plugin: 'zordon', key: 'openCall' } as const, null as string | null)

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
const CHECKOUT_LINE = /^checkout: (.+?) \((?:branch .+|detached @ .+)\)$/

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
        isShared: !isLeaf,
        isPicked: false,
      })
      continue
    }

    const text = DETAIL_LINE.exec(line)?.[1]
    const last = services[services.length - 1]
    if (text === undefined || !last) continue
    const checkout = CHECKOUT_LINE.exec(text)?.[1]
    if (checkout !== undefined) {
      last.checkout = checkout
    } else if (!text.startsWith('checkout:')) {
      last.print.push(text)
    }
  }

  return { services, isAlphaUp }
}

export function parseStatus(exitCode: number, stdout: string, stderr: string): ZordonSnapshot {
  const all = `${stdout}\n${stderr}`
  if (all.includes('no Alphasfile')) {
    return { kind: 'inactive' }
  }

  const workspace = parseWorkspace(stdout)
  const parsed = parseServices(stdout)
  const isAlphaUp = parsed.isAlphaUp
  const services = parsed.services.map(svc => ({
    ...svc,
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
      checkout: x.checkout ?? null,
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

function summary(s: ZordonSnapshot): string | undefined {
  switch (s.kind) {
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

async function refresh($: EngineInterface, sizes: SizeCache): Promise<ZordonSnapshot> {
  let next: ZordonSnapshot
  try {
    const report = await $.process.run(['zordon', 'status', '--format=json'], { timeoutMs: 10_000 })
    const parsed = parseAgentStatus(report.stdout)
    if (parsed !== null) {
      next = parsed
    } else {
      const ran = await $.process.run(['zordon', 'status'], { timeoutMs: 10_000 })
      next = parseStatus(ran.exitCode, ran.stdout, ran.stderr)
    }
  } catch (err) {
    $.ui.log(`zordon: cannot run zordon: ${String(err)}`, { to: 'debug' })
    next = { kind: 'inactive' }
  }

  if (next.kind !== 'inactive' && next.workspace) {
    const dir = next.workspace.stateDir
    const now = await $.clock.now()
    let cached = sizes.get(dir)
    if (!cached || now - cached.at > SIZE_EVERY_MS) {
      cached = { kb: await diskUsageKB($, dir), at: now }
      sizes.set(dir, cached)
    }
    next = { ...next, workspace: { ...next.workspace, sizeKB: cached.kb } }
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
// own: the workspace and the services it picked, the rest its run brings up,
// and the zordon MCP calls Claude made this session. With no workspace a
// single box says so instead.
async function stackView($: EngineInterface, e: RenderEvent, s: ZordonSnapshot) {
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
    footer?: RenderChildren
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
      {p.footer ? (
        <Box justifyContent="space-between" backgroundColor={colors.sub} paddingX={1}>
          {p.footer}
        </Box>
      ) : null}
    </Box>
  )

  const rows = (list: ZordonService[]) => (
    <Box flexDirection="column">
      {list.map(svc => {
        const mark = serviceMark(svc)
        const tags = [mark.note, svc.isShared ? 'shared' : null].filter(Boolean).join(' · ')

        return (
          <Box key={`svc-${svc.name}`} flexDirection="column">
            <Text>
              <Text color={mark.color}>{mark.glyph}</Text> {svc.name}
              {tags ? <Text dimColor> {tags}</Text> : null}
            </Text>
            {svc.print.length > 0 ? (
              <Box paddingLeft={2}>
                <Markdown dimColor text={svc.print.join('\n')} />
              </Box>
            ) : null}
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

  const ws = s.kind === 'inactive' ? null : s.workspace
  const services = s.kind === 'running' || s.kind === 'stopped' ? s.services : []
  const { scope, runtime } = splitScope(services)

  // No workspace: one box saying so, in place of Workspace, Runtime and Logs.
  if (s.kind === 'inactive') {
    return (
      <Box flexDirection="column">
        {header}
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

  const stackBody =
    s.kind === 'error' ? (
      <Markdown text={['❌ **zordon error**', '', '```', s.message, '```'].join('\n')} />
    ) : scope.length > 0 ? (
      rows(scope)
    ) : (
      <Text dimColor>{ws?.name === 'main' ? 'main picks nothing: all runs from the live tree' : 'nothing picked'}</Text>
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
                    wrap="wrap"
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
      {panel({
        key: 'workspace',
        title: 'Workspace',
        aside: ws ? <Text dimColor>{ws.name}</Text> : null,
        border: s.kind === 'error' ? 'red' : border(scope.length > 0 ? scope : services),
        isFolded: isStackFolded,
        onToggle: () => update($, isScopeCollapsed, f => !f),
        body: stackBody,
        footer: ws ? (
          <>
            <Text dimColor>{ws.sizeKB != null ? formatSize(ws.sizeKB) : '…'}</Text>
            {/* Link takes https: alone; a file: link is Markdown's to draw. */}
            <Markdown text={`[Alphasfile](file://${encodeURI(ws.alphasfile)})`} />
          </>
        ) : undefined,
      })}
      {runtime.length === 0
        ? null
        : panel({
            key: 'runtime',
            title: 'Runtime',
            border: border(runtime),
            isFolded: isRuntimeFolded,
            onToggle: () => update($, isRuntimeCollapsed, f => !f),
            body: rows(runtime),
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
        await refresh($, sizes)
      } finally {
        isPolling = false
      }
    }
    void tick().then(async () => {
      if ((await current($)).kind !== 'inactive') await openPane($, false)
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

    await refresh($, sizes)
    const s = await current($)
    const opened = await openPane($, true)
    const where = opened.isPlaced ? 'shown' : `waits: ${opened.reason}`

    return { text: `zordon dashboard ${where}: ${summary(s) ?? `no Alphasfile at or above ${sessionCwd}`}` }
  })

  // The same view inline, as the /zordon:dashboard row in the transcript: what a
  // surface that seats no panes (a phone over Remote Control) still shows.
  on('ui.render', { component: 'CommandOutput', props: { command: DASHBOARD } }, async ($, e, next) => {
    if (e.props.isErrored || e.props.text.endsWith('hidden')) return next(e)

    return stackView($, e, await current($))
  })

  // The stack's view: a pane, docked beside the transcript in fullscreen
  // (inline above the prompt otherwise); the arrows scroll it once focused.
  on('ui.render', { component: 'Pane', requestId: PANE }, async ($, e) => stackView($, e, await current($)))

  // Above the prompt only while something is wrong, one line pointing at the pane.
  on('ui.render', { component: 'AbovePrompt' }, async ($, e, next) => {
    const s = await current($)
    const isBad = s.kind === 'error' || (s.kind === 'running' && s.services.some(x => x.isFailed))
    if (e.props.hasSurvey || !isBad) {
      return next(e)
    }

    const { Box, Button, Text } = $.ui.resolve(e)

    return (
      <Box>
        <Text color="red">{summary(s)} </Text>
        <Button key="open" label="Show" onPress={() => openPane($, true)} />
      </Box>
    )
  })
}
