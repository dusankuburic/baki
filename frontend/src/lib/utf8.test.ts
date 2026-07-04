import {describe, it, expect} from 'vitest'
import {utf8ByteLength} from './utf8'

describe('utf8ByteLength', () => {
  it('matches JS length for pure ASCII', () => {
    expect(utf8ByteLength('hello world')).toBe(11)
  })

  it('counts UTF-8 bytes, not UTF-16 code units, for multibyte content', () => {
    // 😀 is 4 UTF-8 bytes but 2 UTF-16 code units (a surrogate pair).
    // "abc😀def" → 3 + 4 + 3 = 10 bytes; JS .length would report 8.
    expect(utf8ByteLength('abc😀def')).toBe(10)
    expect('abc😀def'.length).toBe(8) // guard: confirms the two units differ
  })

  it('handles CJK and accented content', () => {
    // "日本語" → 3 kanji × 3 bytes = 9 bytes; JS .length reports 3.
    expect(utf8ByteLength('日本語')).toBe(9)
    // "café" → c,a,f (1 each) + é (2 bytes) = 5 bytes; JS .length reports 4.
    expect(utf8ByteLength('café')).toBe(5)
  })

  it('returns 0 for the empty string', () => {
    expect(utf8ByteLength('')).toBe(0)
  })
})
