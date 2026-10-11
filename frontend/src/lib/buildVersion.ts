import {create} from 'zustand'

export const BUILD_VERSION_HEADER = 'X-Baki-Version'

export function normalizeVersion(version: unknown): string {
  if (typeof version !== 'string') return ''
  const v = version.trim().replace(/^v(?=\d)/i, '')
  return v.toLowerCase() === 'dev' ? '' : v
}

export function pageBuildVersion(doc: Document | undefined = globalThis.document): string {
  const content = doc?.querySelector('meta[name="baki-version"]')?.getAttribute('content') ?? ''
  return content.startsWith('__') ? '' : normalizeVersion(content)
}

export const BUNDLE_VERSION = import.meta.env.DEV ? '' : pageBuildVersion()

export function isStaleBundle(bundle: unknown, server: unknown): boolean {
  const a = normalizeVersion(bundle)
  const b = normalizeVersion(server)
  return a !== '' && b !== '' && a !== b
}

export function serverVersionOf(response: Response): string | null {
  try {
    return response.headers.get(BUILD_VERSION_HEADER)
  } catch {
    return null
  }
}

interface BuildVersionState {
  serverVersion: string | null
  offered: boolean
  dismiss: () => void
}

export const useBuildVersionStore = create<BuildVersionState>(set => ({
  serverVersion: null,
  offered: false,
  dismiss: () => set({offered: false}),
}))

const OFFERED_KEY = 'baki.reload-offered'
let decided = ''

function offeredThisSession(version: string): boolean {
  try {
    return sessionStorage.getItem(OFFERED_KEY) === version
  } catch {
    return false
  }
}

function markOffered(version: string): void {
  try {
    sessionStorage.setItem(OFFERED_KEY, version)
  } catch {
  }
}

export function noteServerVersion(server: string | null | undefined, bundle: string = BUNDLE_VERSION): void {
  if (!isStaleBundle(bundle, server)) return
  const version = normalizeVersion(server)
  if (decided === version) return
  decided = version
  if (offeredThisSession(version)) return
  markOffered(version)
  useBuildVersionStore.setState({serverVersion: version, offered: true})
}

export function __resetBuildVersionForTest(): void {
  decided = ''
  useBuildVersionStore.setState({serverVersion: null, offered: false})
}
