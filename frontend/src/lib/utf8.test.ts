import {describe, it, expect} from 'vitest'
import {utf8ByteLength} from './utf8'

describe('utf8ByteLength', () => {
  it('matches JS length for pure ASCII', () => {
    expect(utf8ByteLength('hello world')).toBe(11)
  })

  it('counts UTF-8 bytes, not UTF-16 code units, for multibyte content', () => {
    expect(utf8ByteLength('abc😀def')).toBe(10)
    expect('abc😀def'.length).toBe(8)
  })

  it('handles CJK and accented content', () => {
    expect(utf8ByteLength('日本語')).toBe(9)
    expect(utf8ByteLength('café')).toBe(5)
  })

  it('returns 0 for the empty string', () => {
    expect(utf8ByteLength('')).toBe(0)
  })
})
