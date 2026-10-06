import { expect, test } from 'claude-code/testing'

import { FEATURE, PANE, PR, host, openStack } from './fixture'


test('the stack pane draws header, services and footer on every surface', async ($, on) => {
  host(on, FEATURE)
  await openStack($)
  for (const surface of ['terminal', 'desktop', 'vscode', 'mobile'] as const) {
    const ui = await $.ui.mount({ plugin: 'zordon', surface, ...PANE })
    expect(await ui.find({ type: 'Text', text: /^feature$/ })).toBeDefined()
    expect(await ui.find({ type: 'Text', text: /1\.3 GB/ })).toBeDefined()
    // Link takes only https: spelled as new URL(href).href; anything else
    // refuses the whole tree in a session, which the kit does not check.
    const link = await ui.find({ type: 'Link' })
    expect(link?.props.href).toBe(new URL('https://zordon.io').href)
    expect(await ui.find({ type: 'Text', text: /NOT-THERE/ })).toBeUndefined()
    await ui.unmount()
  }
})

test('the /zordon:dashboard transcript row draws the same view, phone included', async ($, on) => {
  host(on, FEATURE)
  await openStack($)
  for (const surface of ['terminal', 'mobile'] as const) {
    const ui = await $.ui.mount({
      plugin: 'zordon',
      surface,
      component: 'CommandOutput',
      requestId: 'row-1',
      props: { command: 'zordon:dashboard', args: '', text: 'zordon dashboard shown: zordon 4/5 up', isErrored: false },
    })
    expect(await ui.find({ type: 'Text', text: /^feature$/ })).toBeDefined()
    await ui.unmount()
  }
})

test('collapsing the column leaves the Zordon header with the summary', async ($, on) => {
  host(on, FEATURE)
  await openStack($)
  const ui = await $.ui.mount({ plugin: 'zordon', surface: 'terminal', ...PANE })
  expect(await ui.find({ type: 'Text', text: /api/ })).toBeDefined()
  await ui.press({ key: 'collapse' })
  expect(await ui.find({ type: 'Text', text: /api/ })).toBeUndefined()
  expect(await ui.find({ type: 'Text', text: /1\.3 GB/ })).toBeUndefined()
  expect(await ui.find({ type: 'Text', text: /4\/5 up/ })).toBeDefined()
  await ui.press({ key: 'collapse' })
  expect(await ui.find({ type: 'Text', text: /api/ })).toBeDefined()
  await ui.unmount()
})

test('workspace and runtime boxes fold on their own', async ($, on) => {
  host(on, FEATURE)
  await openStack($)
  const ui = await $.ui.mount({ plugin: 'zordon', surface: 'terminal', ...PANE })
  expect(await ui.find({ type: 'Link', text: 'zordon/feature/api' })).toBeDefined()
  expect(await ui.find({ key: 'details-postgres' })).toBeDefined()
  await ui.press({ key: 'fold-workspace' })
  expect(await ui.find({ type: 'Link', text: 'zordon/feature/api' })).toBeUndefined()
  expect(await ui.find({ key: 'details-postgres' })).toBeDefined()
  await ui.press({ key: 'fold-runtime' })
  expect(await ui.find({ key: 'details-postgres' })).toBeUndefined()
  await ui.unmount()
})

test('the column is a Zordon header over Workspace, Runtime and Logs boxes', async ($, on) => {
  host(on, FEATURE)
  await openStack($)
  const ui = await $.ui.mount({ plugin: 'zordon', surface: 'terminal', ...PANE })
  for (const title of [/^Zordon/, /^Workspace$/, /^Runtime$/]) {
    expect(await ui.find({ type: 'Text', text: title })).toBeDefined()
  }
  expect(await ui.find({ type: 'Text', text: /^Logs/ })).toBeUndefined()
  await ui.unmount()
})

test('/zordon:dashboard shows the pane, and hides it when shown', async ($, on) => {
  host(on, FEATURE)

  expect(await openStack($)).toMatch(/^zordon dashboard shown/)
  expect(await openStack($)).toBe('zordon dashboard hidden')
  expect(await openStack($)).toMatch(/^zordon dashboard shown/)
})

test('/zordon:dashboard seats a pane that waits unshown instead of hiding it', async ($, on) => {
  host(on, FEATURE, { isShown: false })

  expect(await openStack($)).toMatch(/^zordon dashboard shown/)
})

test('the workspace groups its services by checkout, each branch linked to its pull request', async ($, on) => {
  host(on, FEATURE)
  await openStack($)
  const ui = await $.ui.mount({ plugin: 'zordon', surface: 'terminal', ...PANE })

  expect((await ui.find({ type: 'Link', text: 'zordon/feature/api' }))?.props.href).toBe(PR.url)
  expect(await ui.find({ type: 'Text', text: /^#7$/ })).toBeDefined()
  expect(await ui.find({ type: 'Text', text: /^zordon\/feature\/worker$/ })).toBeDefined()
  expect(await ui.find({ type: 'Link', text: 'zordon/feature/worker' })).toBeUndefined()
  // a tree inside the workspace by its path there, the project's own by its name
  for (const label of [/^src\/api$/, /^src\/worker$/, /^proj$/]) {
    expect(await ui.find({ type: 'Text', text: label })).toBeDefined()
  }
  expect(await ui.find({ type: 'Text', text: /api cmd\/api/ })).toBeDefined()
  expect(await ui.find({ type: 'Text', text: /gateway gateway/ })).toBeDefined()
  const runtime = (await ui.findAll({ type: 'Button', text: /^▸ / })).map(t => t.props.label)
  expect(runtime).toEqual(['▸ worker', '▸ api', '▸ gateway', '▸ kafka', '▸ postgres'])
  await ui.unmount()
})

test('before zordon first answers the pane reads the stack, it does not call the tree empty', async $ => {
  const ui = await $.ui.mount({ plugin: 'zordon', surface: 'terminal', ...PANE })
  expect(await ui.find({ type: 'Text', text: /Reading the stack/ })).toBeDefined()
  expect(await ui.find({ type: 'Text', text: /Not a workspace/ })).toBeUndefined()
  await ui.unmount()
})

test('the footer keeps size and Alphasfile on one row: both inline, the file opened on press', async ($, on) => {
  const opened: string[] = []
  on('process.run', { argv: ['open'] } as never, ($, e) => {
    opened.push(e.argv[1] ?? '')

    return { value: { exitCode: 0, stdout: '', stderr: '', isStdoutTruncated: false, isStderrTruncated: false } }
  })
  host(on, FEATURE)
  await openStack($)
  const ui = await $.ui.mount({ plugin: 'zordon', surface: 'terminal', ...PANE })

  expect(await ui.find({ type: 'Markdown', text: /Alphasfile/ })).toBeUndefined()
  expect((await ui.find({ key: 'open-alphasfile' }))?.props.label).toBe('Alphasfile')
  await ui.press({ key: 'open-alphasfile' })
  expect(opened).toEqual(['/proj/Alphasfile'])
  await ui.unmount()
})

test('a service opens to its details: the whole print, wrapped, and where its code is', async ($, on) => {
  host(on, FEATURE)
  await openStack($)
  const ui = await $.ui.mount({ plugin: 'zordon', surface: 'terminal', ...PANE })
  expect(await ui.find({ type: 'Text', text: /source \/proj\/workspaces/ })).toBeUndefined()

  await ui.press({ key: 'details-api' })
  expect(await ui.find({ type: 'Text', text: 'print http://localhost:8080' })).toBeDefined()
  expect(await ui.find({ type: 'Text', text: 'branch zordon/feature/api' })).toBeDefined()
  expect(await ui.find({ type: 'Text', text: 'source /proj/workspaces/feature/src/api/cmd/api' })).toBeDefined()
  expect((await ui.find({ key: 'details-api' }))?.props.label).toBe('▾ api')

  await ui.press({ key: 'details-api' })
  expect(await ui.find({ type: 'Text', text: /^source / })).toBeUndefined()
  await ui.unmount()
})
