import { expect, test } from 'claude-code/testing'

import { FEATURE, PANE, host, openStack } from './fixture'
import { parseAgentStatus } from './register'

// Captured from `zordon --agent status` on a running examples/workspace stack.
const AGENT = JSON.stringify({
  workspace: 'agentdemo',
  alphasfile: '/z/examples/workspace/Alphasfile',
  state_dir: '/z/examples/workspace/workspaces/agentdemo',
  state: 'running',
  services: [
    { name: 'serviceA', state: 'ready', picked: true, print: 'http://127.0.0.1:62661/' },
    { name: 'serviceB', state: 'unhealthy', health: 'connection refused' },
    { name: 'postgres', state: 'probing', shared: true },
  ],
})


test('reads zordon --agent status', () => {
  const s = parseAgentStatus(AGENT)
  expect(s?.kind).toBe('running')
  if (s?.kind !== 'running') return
  expect(s.workspace?.stateDir).toBe('/z/examples/workspace/workspaces/agentdemo')
  expect(s.services.map(x => [x.name, x.isUp, x.isFailed, x.isPicked, x.isShared])).toEqual([
    ['serviceA', true, false, true, false],
    ['serviceB', false, true, false, false],
    ['postgres', false, false, false, true],
  ])
  expect(s.services[1]?.state).toBe('unhealthy: connection refused')
})

test('an older zordon printing text is left to the text parser', () => {
  expect(parseAgentStatus('# [abc] /x/Alphasfile (invocation, workspace=main)\n')).toBeNull()
  expect(parseAgentStatus('{"state":"no-alphasfile"}')).toEqual({ kind: 'inactive' })
})

test('zordon MCP calls are logged, and a click opens their output', async ($, on) => {
  const clock = host(on, FEATURE)
  on('tool.call', { tool: 'mcp__zordon__status' }, async () => {
    await clock.advance(120)

    return { result: [{ type: 'text', text: '{"state":"stopped"}' }], text: '{"state":"stopped"}' }
  })
  await openStack($)

  await $.tool.call({ tool: 'mcp__zordon__status', tool_use_id: 'call-1', args: ['-h'] })

  const ui = await $.ui.mount({ plugin: 'zordon', surface: 'terminal', ...PANE })
  const row = await ui.find({ key: 'open-call-1' })
  expect(row?.props.label).toBe('status -h')
  expect(await ui.find({ type: 'Text', text: /120ms/ })).toBeDefined()
  expect(await ui.find({ type: 'Code' })).toBeUndefined()

  await ui.press({ key: 'open-call-1' })
  expect((await ui.find({ type: 'Code' }))?.props.source).toBe('{"state":"stopped"}')

  // The log is a box of its own: folding it leaves the stack alone.
  expect(await ui.find({ type: 'Text', text: /Logs · 1/ })).toBeDefined()
  await ui.press({ key: 'fold-log' })
  expect(await ui.find({ key: 'open-call-1' })).toBeUndefined()
  expect(await ui.find({ type: 'Text', text: /^feature$/ })).toBeDefined()
  await ui.unmount()
})

test('no MCP calls, no log box', async ($, on) => {
  host(on, FEATURE)
  await openStack($)
  const ui = await $.ui.mount({ plugin: 'zordon', surface: 'terminal', ...PANE })
  expect(await ui.find({ type: 'Text', text: /Logs · / })).toBeUndefined()
  await ui.unmount()
})

test("the plugin's own zordon server is logged under its plugin-scoped name", async ($, on) => {
  host(on, FEATURE)
  on('tool.call', { tool: 'mcp__plugin_zordon_zordon__plan' }, () => ({ result: [], text: 'plan' }))
  await openStack($)

  await $.tool.call({ tool: 'mcp__plugin_zordon_zordon__plan', tool_use_id: 'call-2', args: [] })

  const ui = await $.ui.mount({ plugin: 'zordon', surface: 'terminal', ...PANE })
  expect((await ui.find({ key: 'open-call-2' }))?.props.label).toBe('plan')
  await ui.unmount()
})

test('with no workspace there is no Logs box, whatever was called', async ($, on) => {
  host(on, '{"state":"no-alphasfile"}')
  on('tool.call', { tool: 'mcp__zordon__status' }, () => ({ result: [], text: 'no Alphasfile' }))
  await openStack($)

  await $.tool.call({ tool: 'mcp__zordon__status', tool_use_id: 'call-3', args: [] })

  const ui = await $.ui.mount({ plugin: 'zordon', surface: 'terminal', ...PANE })
  expect(await ui.find({ type: 'Text', text: /No Alphasfile/ })).toBeDefined()
  for (const title of [/^Workspace$/, /^Runtime$/, /^Logs/]) {
    expect(await ui.find({ type: 'Text', text: title })).toBeUndefined()
  }
  await ui.unmount()
})
