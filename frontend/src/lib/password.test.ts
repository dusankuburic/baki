import {describe, it, expect} from 'vitest'
import {checkPassword, PASSWORD_MAX_LENGTH, PASSWORD_MIN_LENGTH} from './password'

describe('checkPassword', () => {
  it('mirrors the server policy: 12–72 bytes and three of four character classes', () => {
    expect(checkPassword('correct-horse-battery').valid).toBe(false) // lowercase + symbol only
    expect(checkPassword('Correct-horse-1').valid).toBe(true)
    expect(checkPassword('CorrectHorse1').valid).toBe(true) // no symbol, still three classes
    expect(checkPassword('Short1!').valid).toBe(false)
  })

  it('reports each rule separately so the form can show what is still missing', () => {
    const weak = checkPassword('abc')
    expect(weak.longEnough).toBe(false)
    expect(weak.classes).toEqual({lowercase: true, uppercase: false, digit: false, symbol: false})
    expect(weak.classCount).toBe(1)
    expect(weak.enoughClasses).toBe(false)

    const strong = checkPassword('Tr0ub4dor&3xyz')
    expect(strong.longEnough).toBe(true)
    expect(strong.classCount).toBe(4)
    expect(strong.valid).toBe(true)
  })

  it('counts bytes, not characters, like the server (bcrypt truncates at 72 bytes)', () => {
    const emoji = '😀'.repeat(3) // 12 bytes, 3 characters, all "symbols"
    expect(checkPassword(emoji).byteLength).toBe(PASSWORD_MIN_LENGTH)
    expect(checkPassword(emoji).longEnough).toBe(true)

    const tooLong = 'Aa1'.repeat(24) + 'x' // 73 bytes
    expect(checkPassword(tooLong).byteLength).toBe(PASSWORD_MAX_LENGTH + 1)
    expect(checkPassword(tooLong).tooLong).toBe(true)
    expect(checkPassword(tooLong).valid).toBe(false)
    expect(checkPassword('Aa1'.repeat(24)).valid).toBe(true)
  })

  it('treats spaces and uncased letters as symbols, as unicode.IsLower/IsUpper/IsDigit do', () => {
    const check = checkPassword('abc def 12345')
    expect(check.classes.symbol).toBe(true)
    expect(check.classCount).toBe(3)
    expect(check.valid).toBe(true)
  })
})
