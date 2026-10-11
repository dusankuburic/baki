import {describe, it, expect} from 'vitest'
import {emailErrorKey, httpUrlErrorKey} from './validators'
import {EMAIL_MAX_LENGTH} from './constraints'

describe('emailErrorKey', () => {
  it('accepts ordinary addresses and passes blank through to required-field gating', () => {
    expect(emailErrorKey('user@example.com')).toBeNull()
    expect(emailErrorKey('  first.last@sub.example.co  ')).toBeNull()
    expect(emailErrorKey('')).toBeNull()
    expect(emailErrorKey('   ')).toBeNull()
  })

  it('flags the obvious mistakes the server would reject', () => {
    expect(emailErrorKey('no-at-sign.example.com')).toBe('errors.invalidEmail')
    expect(emailErrorKey('a@b@c')).toBe('errors.invalidEmail')
    expect(emailErrorKey('user@example')).toBe('errors.invalidEmail')
    expect(emailErrorKey('first last@example.com')).toBe('errors.invalidEmail')
  })

  it('caps length in bytes like the server len() check', () => {
    expect(emailErrorKey(`${'a'.repeat(EMAIL_MAX_LENGTH - 12)}@example.com`)).toBeNull()
    expect(emailErrorKey(`${'a'.repeat(EMAIL_MAX_LENGTH - 11)}@example.com`)).toBe('errors.emailTooLong')
    expect(emailErrorKey(`${'é'.repeat(123)}@example.com`)).toBe('errors.emailTooLong')
  })
})

describe('httpUrlErrorKey', () => {
  it('accepts http(s) URLs and passes blank through', () => {
    expect(httpUrlErrorKey('https://example.com/avatar.png')).toBeNull()
    expect(httpUrlErrorKey('http://127.0.0.1:8080/x')).toBeNull()
    expect(httpUrlErrorKey('')).toBeNull()
  })

  it('rejects unparseable input and non-http(s) schemes like the profile handler', () => {
    expect(httpUrlErrorKey('not a url')).toBe('errors.invalidUrl')
    expect(httpUrlErrorKey('example.com/no-scheme')).toBe('errors.invalidUrl')
    expect(httpUrlErrorKey('ftp://example.com/f')).toBe('errors.invalidUrl')
    expect(httpUrlErrorKey('javascript:alert(1)')).toBe('errors.invalidUrl')
  })

  it('caps length in bytes like the server len() check', () => {
    expect(httpUrlErrorKey(`https://example.com/${'a'.repeat(2020)}`)).toBeNull()
    expect(httpUrlErrorKey(`https://example.com/${'a'.repeat(2100)}`)).toBe('errors.urlTooLong')
  })
})
