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
