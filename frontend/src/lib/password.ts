// Client-side mirror of validatePasswordStrength in
// internal/api/handlers_auth.go, so the change-password form can show the
// rules while the user types instead of relaying the server's 400 as a toast.
// Lengths are UTF-8 BYTES like the server's len(pw) — bcrypt ignores
// everything past 72 bytes, so that cap is what actually protects the account.
import {utf8ByteLength} from './utf8'

export const PASSWORD_MIN_LENGTH = 12
export const PASSWORD_MAX_LENGTH = 72
export const PASSWORD_MIN_CLASSES = 3

export type PasswordClass = 'lowercase' | 'uppercase' | 'digit' | 'symbol'
export const PASSWORD_CLASSES: readonly PasswordClass[] = ['lowercase', 'uppercase', 'digit', 'symbol']

export interface PasswordCheck {
  byteLength: number
  longEnough: boolean
  tooLong: boolean
  classes: Record<PasswordClass, boolean>
  classCount: number
  enoughClasses: boolean
  valid: boolean
}

// The server switches on unicode.IsLower / IsUpper / IsDigit and counts every
// other rune (punctuation, spaces, uncased scripts) as a symbol; the Unicode
// property escapes below are the same categories (Ll, Lu, Nd).
function classOf(ch: string): PasswordClass {
  if (/\p{Ll}/u.test(ch)) return 'lowercase'
  if (/\p{Lu}/u.test(ch)) return 'uppercase'
  if (/\p{Nd}/u.test(ch)) return 'digit'
  return 'symbol'
}

export function checkPassword(pw: string): PasswordCheck {
  const classes: Record<PasswordClass, boolean> = {lowercase: false, uppercase: false, digit: false, symbol: false}
  for (const ch of pw) classes[classOf(ch)] = true
  const classCount = PASSWORD_CLASSES.filter(c => classes[c]).length
  const byteLength = utf8ByteLength(pw)
  const longEnough = byteLength >= PASSWORD_MIN_LENGTH
  const tooLong = byteLength > PASSWORD_MAX_LENGTH
  const enoughClasses = classCount >= PASSWORD_MIN_CLASSES
  return {
    byteLength,
    longEnough,
    tooLong,
    classes,
    classCount,
    enoughClasses,
    valid: longEnough && !tooLong && enoughClasses,
  }
}
