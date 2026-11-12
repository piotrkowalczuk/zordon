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
  // only what is in the workspace's src/; gateway builds from the project root
  for (const label of [/^src\/api$/, /^src\/worker$/]) {
    expect(await ui.find({ type: 'Text', text: label })).toBeDefined()
  }
  expect(await ui.find({ type: 'Text', text: /^proj$/ })).toBeUndefined()
  expect(await ui.find({ type: 'Text', text: /api cmd\/api/ })).toBeDefined()
  expect(await ui.find({ type: 'Text', text: /gateway gateway/ })).toBeUndefined()
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

test('shows an Update box in the pane when a newer release exists, and hides it when dismissed', async ($, on) => {
  host(on, FEATURE, { isShown: true }, { cli: 'v0.28.0', latestTag: 'v99.0.0', binPath: '/Users/dev/go/bin/zordon' })
  await openStack($)
  const ui = await $.ui.mount({ plugin: 'zordon', surface: 'terminal', ...PANE })

  expect(await ui.find({ type: 'Text', text: /Update · v99\.0\.0/ })).toBeDefined()
  expect(await ui.find({ type: 'Text', text: /cli v0\.28\.0 → v99\.0\.0/ })).toBeDefined()
  expect(await ui.find({ type: 'Text', text: /download release → \/Users\/dev\/go\/bin/ })).toBeDefined()
  expect(await ui.find({ type: 'Text', text: /claude plugin update zordon@zordon/ })).toBeDefined()
  expect((await ui.find({ key: 'run-update-pane' }))?.props.label).toBe('Update')
  expect((await ui.find({ type: 'Link', text: 'release' }))?.props.href).toBe(
    'https://github.com/piotrkowalczuk/zordon/releases/tag/v99.0.0',
  )

  await ui.press({ key: 'dismiss-update-pane' })
  expect(await ui.find({ type: 'Text', text: /Update · v99\.0\.0/ })).toBeUndefined()
  await ui.unmount()
})

test('pressing Update downloads the release tarball for non-Homebrew installs and updates the plugin', async ($, on) => {
  const ran: string[][] = []
  host(on, FEATURE, { isShown: true }, { cli: 'v0.28.0', latestTag: 'v99.0.0', binPath: '/Users/dev/go/bin/zordon', ran })
  await openStack($)
  const ui = await $.ui.mount({ plugin: 'zordon', surface: 'terminal', ...PANE })

  await ui.press({ key: 'run-update-pane' })
  expect(await ui.find({ type: 'Text', text: /Update · v99\.0\.0/ })).toBeUndefined()
  expect(ran.some(argv => argv[0] === 'sh' && argv[4] === '99.0.0' && argv[5] === 'darwin' && argv[6] === 'arm64' && argv[7] === '/Users/dev/go/bin')).toBe(true)
  expect(ran.some(argv => argv[0] === 'claude' && argv[1] === 'plugin' && argv[2] === 'update' && argv[3] === 'zordon@zordon')).toBe(true)
  expect(ran.some(argv => argv[0] === 'brew')).toBe(false)
  await ui.unmount()
})

test('pressing Update uses Homebrew when the installed binary is managed by brew', async ($, on) => {
  const ran: string[][] = []
  host(on, FEATURE, { isShown: true }, { cli: 'v0.28.0', latestTag: 'v99.0.0', binPath: '/opt/homebrew/bin/zordon', ran })
  await openStack($)
  const ui = await $.ui.mount({ plugin: 'zordon', surface: 'terminal', ...PANE })

  await ui.press({ key: 'run-update-pane' })
  expect(await ui.find({ type: 'Text', text: /Update · v99\.0\.0/ })).toBeUndefined()
  expect(ran.some(argv => argv[0] === 'brew' && argv[1] === 'upgrade' && argv[2] === 'piotrkowalczuk/tap/zordon')).toBe(true)
  expect(ran.some(argv => argv[0] === 'sh')).toBe(false)
  await ui.unmount()
})

test('shows the Update box in an opened dashboard pane even outside a workspace', async ($, on) => {
  host(on, '{"state":"no-alphasfile"}', { isShown: true }, { cli: 'v0.28.0', latestTag: 'v99.0.0' })
  await openStack($)
  const ui = await $.ui.mount({ plugin: 'zordon', surface: 'terminal', ...PANE })

  expect(await ui.find({ type: 'Text', text: /Not a workspace/ })).toBeDefined()
  expect(await ui.find({ type: 'Text', text: /Update · v99\.0\.0/ })).toBeDefined()
  await ui.unmount()
})
