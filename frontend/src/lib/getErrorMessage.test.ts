import {describe, it, expect} from 'vitest'
import {ApiError} from '@/api/client'
import {getErrorMessage, getErrorCode} from './getErrorMessage'

describe('getErrorMessage', () => {
  it('returns the message of an Error, including ApiError', () => {
    expect(getErrorMessage(new Error('boom'))).toBe('boom')
    expect(getErrorMessage(new ApiError('denied', 403, 'PERM'))).toBe('denied')
    expect(getErrorMessage(new TypeError('x is not a function'))).toBe('x is not a function')
  })

  it('returns the site fallback for non-Error values', () => {
    expect(getErrorMessage('oops', 'Failed to load')).toBe('Failed to load')
    expect(getErrorMessage(undefined, 'Failed to load')).toBe('Failed to load')
    expect(getErrorMessage({code: 500}, 'Failed to load')).toBe('Failed to load')
  })

  it('stringifies non-Error values when no fallback is given', () => {
    expect(getErrorMessage('oops')).toBe('oops')
    expect(getErrorMessage(42)).toBe('42')
    expect(getErrorMessage(null)).toBe('null')
    expect(getErrorMessage(undefined)).toBe('undefined')
  })

  it('prefers the Error message over the fallback, like the idiom it replaces', () => {
    expect(getErrorMessage(new Error('real cause'), 'Failed to load')).toBe('real cause')
  })
})

describe('getErrorCode', () => {
  it('extracts the ApiError machine code', () => {
    expect(getErrorCode(new ApiError('m', 400, 'AI_BUDGET_EXCEEDED'))).toBe('AI_BUDGET_EXCEEDED')
  })

  it('returns null for non-ApiError values and absent codes', () => {
    expect(getErrorCode(new Error('plain'))).toBeNull()
    expect(getErrorCode(new ApiError('m', 500))).toBeNull()
    expect(getErrorCode('nope')).toBeNull()
    expect(getErrorCode(undefined)).toBeNull()
  })
})
