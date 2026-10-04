import { expect, test } from 'claude-code/testing'

import { FEATURE, PANE, host, openStack } from './fixture'


test('the stack pane draws header, services and footer on every surface', async ($, on) => {
  host(on, FEATURE)
  await openStack($)
  for (const surface of ['terminal', 'desktop', 'vscode', 'mobile'] as const) {
    const ui = await $.ui.mount({ plugin: 'zordon', surface, ...PANE })
    expect(await ui.find({ type: 'Text', text: /Workspace: feature/ })).toBeDefined()
    expect(await ui.find({ type: 'Text', text: /1\.3 GB/ })).toBeDefined()
    // Link takes only https: spelled as new URL(href).href; anything else
    // refuses the whole tree in a session, which the kit does not check.
    const link = await ui.find({ type: 'Link' })
    expect(link?.props.href).toBe(new URL('https://zordon.io').href)
    expect(await ui.find({ type: 'Text', text: /NOT-THERE/ })).toBeUndefined()
    await ui.unmount()
  }
})

test('the /zordon transcript row draws the same view, phone included', async ($, on) => {
  host(on, FEATURE)
  await openStack($)
  for (const surface of ['terminal', 'mobile'] as const) {
    const ui = await $.ui.mount({
      plugin: 'zordon',
      surface,
      component: 'CommandOutput',
      requestId: 'row-1',
      props: { command: 'zordon', args: '', text: 'zordon 3/4 up', isErrored: false },
    })
    expect(await ui.find({ type: 'Text', text: /Workspace: feature/ })).toBeDefined()
    await ui.unmount()
  }
})

test('the collapse button leaves header and footer alone', async ($, on) => {
  host(on, FEATURE)
  await openStack($)
  const ui = await $.ui.mount({ plugin: 'zordon', surface: 'terminal', ...PANE })
  expect(await ui.find({ type: 'Text', text: /api/ })).toBeDefined()
  await ui.press({ key: 'collapse' })
  expect(await ui.find({ type: 'Text', text: /api/ })).toBeUndefined()
  expect(await ui.find({ type: 'Text', text: /Workspace: feature/ })).toBeDefined()
  expect(await ui.find({ type: 'Text', text: /1\.3 GB/ })).toBeDefined()
  await ui.press({ key: 'collapse' })
  expect(await ui.find({ type: 'Text', text: /api/ })).toBeDefined()
  await ui.unmount()
})

test('scope and runtime fold on their own', async ($, on) => {
  host(on, FEATURE)
  await openStack($)
  const ui = await $.ui.mount({ plugin: 'zordon', surface: 'terminal', ...PANE })
  expect(await ui.find({ type: 'Text', text: /api/ })).toBeDefined()
  expect(await ui.find({ type: 'Text', text: /postgres/ })).toBeDefined()
  await ui.press({ key: 'fold-scope' })
  expect(await ui.find({ type: 'Text', text: /api/ })).toBeUndefined()
  expect(await ui.find({ type: 'Text', text: /postgres/ })).toBeDefined()
  await ui.press({ key: 'fold-runtime' })
  expect(await ui.find({ type: 'Text', text: /postgres/ })).toBeUndefined()
  await ui.unmount()
})
