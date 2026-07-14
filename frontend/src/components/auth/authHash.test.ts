import {describe, it, expect, beforeEach, afterEach} from 'vitest'
import {parseRecoveryHash, clearRecoveryHash} from './authHash'

function setHash(h: string) {
  window.history.replaceState(null, '', '/' + (h ? '#' + h : ''))
}

describe('parseRecoveryHash', () => {
  beforeEach(() => setHash(''))
  afterEach(() => setHash(''))

  it('returns empty when no recovery params are present', () => {
    setHash('ssoTicket=abc')
    expect(parseRecoveryHash()).toEqual({})
  })

  it('extracts the reset token without mutating the fragment', () => {
    setHash('resetPassword=RTOKEN')
    expect(parseRecoveryHash()).toEqual({resetToken: 'RTOKEN'})
    // Pure: still present until explicitly cleared (StrictMode-safe).
    expect(window.location.hash).toBe('#resetPassword=RTOKEN')
  })

  it('extracts the verify token', () => {
    setHash('verifyEmail=VTOKEN')
    expect(parseRecoveryHash()).toEqual({verifyToken: 'VTOKEN'})
  })

  it('extracts the invite token', () => {
    setHash('invite=ITOKEN')
    expect(parseRecoveryHash()).toEqual({inviteToken: 'ITOKEN'})
  })

  it('clearRecoveryHash strips the fragment', () => {
    setHash('resetPassword=RTOKEN')
    clearRecoveryHash()
    expect(window.location.hash).toBe('')
  })

  // StrictMode double-invokes useState initializers. parseRecoveryHash must be
  // pure so two back-to-back calls both see the token — the original bug stripped
  // the hash in the initializer, so the 2nd invocation returned empty.
  it('is idempotent across repeated calls (StrictMode-safe)', () => {
    setHash('resetPassword=RTOKEN')
    const first = parseRecoveryHash()
    const second = parseRecoveryHash()
    expect(first).toEqual({resetToken: 'RTOKEN'})
    expect(second).toEqual({resetToken: 'RTOKEN'})
    expect(window.location.hash).toBe('#resetPassword=RTOKEN')
  })
})
