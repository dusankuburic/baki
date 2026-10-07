import {describe, it, expect, beforeEach} from 'vitest'
import {
  BUILD_VERSION_HEADER,
  BUNDLE_VERSION,
  __resetBuildVersionForTest,
  isStaleBundle,
  pageBuildVersion,
  normalizeVersion,
  noteServerVersion,
  serverVersionOf,
  useBuildVersionStore,
} from './buildVersion'

beforeEach(() => {
  sessionStorage.clear()
  __resetBuildVersionForTest()
})

describe('comparing builds', () => {
  it('reads a version trimmed and without a leading "v", and "dev" or nothing as no build', () => {
    expect(normalizeVersion(' v1.4.0-3-gabc1234 ')).toBe('1.4.0-3-gabc1234')
    expect(normalizeVersion('version9')).toBe('version9')
    expect(normalizeVersion('dev')).toBe('')
    expect(normalizeVersion('')).toBe('')
    expect(normalizeVersion(undefined)).toBe('')
    expect(normalizeVersion(42)).toBe('')
  })

  it('calls the bundle stale only when both sides name a build and they differ', () => {
    expect(isStaleBundle('v1.4.0-3-gabc', 'v1.4.0-5-gdef')).toBe(true)
    expect(isStaleBundle('v1.4.0', '1.4.0')).toBe(false)
    // A dev or unstamped build never nags, on either side.
    expect(isStaleBundle('', 'v1.4.0')).toBe(false)
    expect(isStaleBundle('dev', 'v1.4.0')).toBe(false)
    expect(isStaleBundle('v1.4.0', null)).toBe(false)
    expect(isStaleBundle('v1.4.0', '')).toBe(false)
  })

  it('is a dev build under the test runner: the bundle names no build, so nothing compares', () => {
    expect(BUNDLE_VERSION).toBe('')
    noteServerVersion('v9.9.9')
    expect(useBuildVersionStore.getState().offered).toBe(false)
  })

  it("reads the page's build from its baki-version meta, and an unstamped placeholder as no build", () => {
    const page = (content: string | null) =>
      new DOMParser().parseFromString(
        content === null
          ? '<html><head></head></html>'
          : `<html><head><meta name="baki-version" content="${content}"></head></html>`,
        'text/html',
      )
    expect(pageBuildVersion(page('v1.4.0-3-gabc1234'))).toBe('1.4.0-3-gabc1234')
    expect(pageBuildVersion(page('__BAKI_VERSION__'))).toBe('')
    expect(pageBuildVersion(page(null))).toBe('')
    expect(pageBuildVersion(undefined)).toBe('')
  })

  it("reads the response's header, and nothing from a response without headers", () => {
    expect(serverVersionOf(new Response('{}', {headers: {[BUILD_VERSION_HEADER]: 'v2.0.0'}}))).toBe('v2.0.0')
    expect(serverVersionOf(new Response('{}'))).toBeNull()
    expect(serverVersionOf({} as Response)).toBeNull()
  })
})

describe('offering the reload', () => {
  it('offers it on the first response from another build, naming that build', () => {
    noteServerVersion('v1.0.0', 'v1.0.0')
    noteServerVersion(null, 'v1.0.0')
    expect(useBuildVersionStore.getState().offered).toBe(false)
    noteServerVersion('v1.1.0', 'v1.0.0')
    expect(useBuildVersionStore.getState()).toMatchObject({offered: true, serverVersion: '1.1.0'})
  })

  it('stays dismissed for the responses that follow from the same build', () => {
    noteServerVersion('v1.1.0', 'v1.0.0')
    useBuildVersionStore.getState().dismiss()
    noteServerVersion('v1.1.0', 'v1.0.0')
    expect(useBuildVersionStore.getState().offered).toBe(false)
  })

  it('offers it once per server build: a reload that lands on the old bundle again is not asked again', () => {
    noteServerVersion('v1.1.0', 'v1.0.0')
    expect(useBuildVersionStore.getState().offered).toBe(true)
    // The page reloads (same tab session) and still runs the old bundle.
    __resetBuildVersionForTest()
    noteServerVersion('v1.1.0', 'v1.0.0')
    expect(useBuildVersionStore.getState().offered).toBe(false)
  })

  it('offers a later deploy again, in the same page after a dismissal and in the same tab after a reload', () => {
    noteServerVersion('v1.1.0', 'v1.0.0')
    useBuildVersionStore.getState().dismiss()
    noteServerVersion('v1.2.0', 'v1.0.0')
    expect(useBuildVersionStore.getState()).toMatchObject({offered: true, serverVersion: '1.2.0'})
    // Reloaded onto the 1.2.0 bundle; then 1.3.0 ships while the tab stays open.
    __resetBuildVersionForTest()
    noteServerVersion('v1.2.0', 'v1.2.0')
    expect(useBuildVersionStore.getState().offered).toBe(false)
    noteServerVersion('v1.3.0', 'v1.2.0')
    expect(useBuildVersionStore.getState()).toMatchObject({offered: true, serverVersion: '1.3.0'})
  })
})
