import {describe, it, expect, afterEach, vi} from 'vitest'
import {uuid} from './uuid'

const realCrypto = globalThis.crypto

afterEach(() => {
  Object.defineProperty(globalThis, 'crypto', {value: realCrypto, writable: true, configurable: true})
  vi.restoreAllMocks()
})

const V4 = /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/

describe('uuid', () => {
  it('delegates to crypto.randomUUID when available', () => {
    const spy = vi.spyOn(globalThis.crypto, 'randomUUID')
    expect(uuid()).toMatch(V4)
    expect(spy).toHaveBeenCalled()
  })

  // Regression: outside a secure context (plain-HTTP LAN deploy) randomUUID is
  // undefined, and every chat id generation threw a TypeError.
  it('falls back to getRandomValues when randomUUID is unavailable', () => {
    Object.defineProperty(globalThis, 'crypto', {
      value: {getRandomValues: (a: Uint8Array) => realCrypto.getRandomValues(a)},
      writable: true,
      configurable: true,
    })
    expect(() => uuid()).not.toThrow()
    expect(uuid()).toMatch(V4)
  })

  it('produces distinct ids on the fallback path', () => {
    Object.defineProperty(globalThis, 'crypto', {
      value: {getRandomValues: (a: Uint8Array) => realCrypto.getRandomValues(a)},
      writable: true,
      configurable: true,
    })
    const ids = new Set(Array.from({length: 500}, () => uuid()))
    expect(ids.size).toBe(500)
  })
})
