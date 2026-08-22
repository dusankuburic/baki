import {describe, it, expect, beforeEach, afterEach} from 'vitest'
import {getPlatform, notifyIfBackground, setPlatformForTest, resetPlatformForTest} from './usePlatform'
import type {PlatformAdapter, NotificationOptions} from '@/platform/types'

let captured: NotificationOptions | null = null
const fakeAdapter = {
  showNotification: async (opts: NotificationOptions) => {
    captured = opts
  },
} as unknown as PlatformAdapter

describe('notifyIfBackground', () => {
  const visibilityDescriptor = Object.getOwnPropertyDescriptor(Document.prototype, 'visibilityState')

  beforeEach(() => {
    captured = null
    setPlatformForTest(fakeAdapter)
  })

  afterEach(() => {
    resetPlatformForTest()
    // Restore the original visibilityState getter (jsdom defines it on the prototype).
    if (visibilityDescriptor) {
      Object.defineProperty(Document.prototype, 'visibilityState', visibilityDescriptor)
    }
  })

  function setVisibility(state: 'visible' | 'hidden') {
    Object.defineProperty(Document.prototype, 'visibilityState', {
      value: state,
      configurable: true,
    })
  }

  it('is a no-op while the document is visible', async () => {
    setVisibility('visible')
    await notifyIfBackground({title: 't', body: 'b'})
    expect(captured).toBeNull()
  })

  it('fires the notification when the document is hidden', async () => {
    setVisibility('hidden')
    await notifyIfBackground({title: 'Analysis', body: '3 findings'})
    expect(captured).toEqual({title: 'Analysis', body: '3 findings'})
  })

  it('swallows adapter errors so the caller is never disrupted', async () => {
    setVisibility('hidden')
    const failing = {
      showNotification: async () => {
        throw new Error('boom')
      },
    } as unknown as PlatformAdapter
    setPlatformForTest(failing)
    await expect(notifyIfBackground({title: 't', body: 'b'})).resolves.toBeUndefined()
  })
})

describe('getPlatform', () => {
  it('returns a cached singleton once built', () => {
    resetPlatformForTest()
    const a = getPlatform()
    const b = getPlatform()
    expect(a).toBe(b)
    resetPlatformForTest()
  })
})
