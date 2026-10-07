import {create} from 'zustand'

// A tab kept open across a deploy goes on running the old bundle against the
// new API. Every server response names the build it runs (X-Baki-Version,
// internal/api/router.go); when that differs from the build this bundle is,
// the shell offers a reload (components/layout/ReloadBanner), once per server build.

export const BUILD_VERSION_HEADER = 'X-Baki-Version'

/** A version as compared: trimmed, a leading "v" dropped; '' when it names no build. */
export function normalizeVersion(version: unknown): string {
  if (typeof version !== 'string') return ''
  const v = version.trim().replace(/^v(?=\d)/i, '')
  return v.toLowerCase() === 'dev' ? '' : v
}

// The page's build, from index.html's baki-version meta, which the image build
// stamps with the same VERSION the server gets (Dockerfile). It lives in the
// page rather than in a chunk so a new version renames no chunk. Unstamped, the
// meta still holds its placeholder, which names no build.
export function pageBuildVersion(doc: Document | undefined = globalThis.document): string {
  const content = doc?.querySelector('meta[name="baki-version"]')?.getAttribute('content') ?? ''
  return content.startsWith('__') ? '' : normalizeVersion(content)
}

// The build this bundle is. A dev server's bundle changes under HMR and is no
// build at all, so it never compares, nor does an unstamped page. The desktop
// app ships bundle and sidecar together and stamps neither, so it compares only
// if both ever carry real, different builds.
export const BUNDLE_VERSION = import.meta.env.DEV ? '' : pageBuildVersion()

/** Whether the server runs another build than the bundle: never while either names none. */
export function isStaleBundle(bundle: unknown, server: unknown): boolean {
  const a = normalizeVersion(bundle)
  const b = normalizeVersion(server)
  return a !== '' && b !== '' && a !== b
}

/** The build a response names; null without the header (an outer layer's answer, the worker's offline reply). */
export function serverVersionOf(response: Response): string | null {
  try {
    return response.headers.get(BUILD_VERSION_HEADER)
  } catch {
    return null
  }
}

interface BuildVersionState {
  /** The server's build, once a response named one this bundle is not; null until then. */
  serverVersion: string | null
  /** The reload banner is up. */
  offered: boolean
  dismiss: () => void
}

export const useBuildVersionStore = create<BuildVersionState>(set => ({
  serverVersion: null,
  offered: false,
  dismiss: () => set({offered: false}),
}))

// Once per server build: a dismissal holds for the responses that follow, and a
// reload that lands on the same old bundle again (a cache in between) is not
// offered another reload for that build, but a later deploy is offered again,
// also in a tab that lives through several. Without storage, once per page.
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
    // The page-level flag still holds for this page.
  }
}

/** Takes the build a server response named; the API client calls it for every response. */
export function noteServerVersion(server: string | null | undefined, bundle: string = BUNDLE_VERSION): void {
  if (!isStaleBundle(bundle, server)) return
  const version = normalizeVersion(server)
  if (decided === version) return
  decided = version
  if (offeredThisSession(version)) return
  markOffered(version)
  useBuildVersionStore.setState({serverVersion: version, offered: true})
}

/** Tests only: forget what this page decided. */
export function __resetBuildVersionForTest(): void {
  decided = ''
  useBuildVersionStore.setState({serverVersion: null, offered: false})
}
