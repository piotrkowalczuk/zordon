export type ZordonService = {
  name: string
  toolchain: string
  state: string
  isUp: boolean
  isFailed: boolean
  print: string[]
  checkout: string | null
  branch: string | null
  sourceDir: string | null
  isShared: boolean
  isPicked: boolean
}

export type ZordonWorkspace = {
  name: string
  alphasfile: string
  stateDir: string
  sizeKB: number | null
}

export type DiffFile = {
  path: string
  added: number
  removed: number
  hunks: string | null
  note: string | null
}

export type DiffApp = { name: string; changes: number }

export type DiffView = {
  apps: DiffApp[]
  app: string | null
  files: DiffFile[]
  more: number
  isLoading: boolean
}

export type Checkout = {
  path: string
  label: string
  branch: string | null
  pr: { url: string; number: number } | null
  apps: { name: string; dir: string | null }[]
}

export type McpCall = {
  id: string
  command: string
  args: string
  startedAt: number
  durationMs: number | null
  isError: boolean
  output: string
}

export type ZordonSnapshot =
  | { kind: 'loading' }
  | { kind: 'inactive' }
  | { kind: 'error'; message: string; workspace: ZordonWorkspace | null }
  | { kind: 'stopped'; services: ZordonService[]; workspace: ZordonWorkspace | null }
  | { kind: 'running'; services: ZordonService[]; workspace: ZordonWorkspace | null }

declare module 'claude-code' {
  interface PluginState {
    'zordon': { snapshot: ZordonSnapshot; diff: DiffView; isCollapsed: boolean; isScopeCollapsed: boolean; isRuntimeCollapsed: boolean; isLogCollapsed: boolean; calls: McpCall[]; openCall: string | null; checkouts: Checkout[] }
  }
}
