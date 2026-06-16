import { describe, it, expect } from 'vitest'
import { decodeJwtPayload } from './jwt'

// Build a JWT-shaped "<header>.<payload>.<sig>" whose payload is real base64url
// ('+'→'-', '/'→'_', padding stripped) exactly as a signing library emits it.
function jwtWith(payload: Record<string, unknown>): string {
  const seg = btoa(JSON.stringify(payload))
    .replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '')
  return `header.${seg}.signature`
}

describe('decodeJwtPayload', () => {
  it('decodes a base64url payload containing - and _ (which raw atob rejects)', () => {
    // This payload is engineered so its base64url segment contains BOTH '-' and
    // '_'; the test would fail if the helper still used a bare atob().
    const payload = { exp: 1700000000, role: 'admin', s: '????>>>>' }
    const token = jwtWith(payload)
    expect(token.split('.')[1]).toMatch(/[-_]/) // precondition
    expect(decodeJwtPayload(token)).toEqual(payload)
  })

  it('round-trips a typical access-token payload with stripped padding', () => {
    const payload = { uid: 'a3f9-c2', email: 'u@x.io', role: 'member', exp: 1893456000 }
    expect(decodeJwtPayload(jwtWith(payload))).toEqual(payload)
  })

  it('returns null for a non-JWT string (e.g. the local-mode static token)', () => {
    expect(decodeJwtPayload('not-a-jwt')).toBeNull()
    expect(decodeJwtPayload('only.two')).toBeNull()
  })

  it('returns null when the payload is not valid base64 or not a JSON object', () => {
    expect(decodeJwtPayload('header.@@@not-base64@@@.sig')).toBeNull()
    expect(decodeJwtPayload('header.bm90LWpzb24.sig')).toBeNull() // base64 of "not-json"
  })
})
