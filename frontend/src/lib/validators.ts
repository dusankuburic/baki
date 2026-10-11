import {utf8ByteLength} from './utf8'
import {AVATAR_URL_MAX_LENGTH, EMAIL_MAX_LENGTH} from './constraints'

export type CommonFieldErrorKey =
  | 'errors.invalidEmail'
  | 'errors.emailTooLong'
  | 'errors.invalidUrl'
  | 'errors.urlTooLong'

const EMAIL_RE = /^[^\s@]+@[^\s@]+\.[^\s@]+$/

export function emailErrorKey(value: string): CommonFieldErrorKey | null {
  const v = value.trim()
  if (!v) return null
  if (utf8ByteLength(v) > EMAIL_MAX_LENGTH) return 'errors.emailTooLong'
  return EMAIL_RE.test(v) ? null : 'errors.invalidEmail'
}

export function httpUrlErrorKey(value: string): CommonFieldErrorKey | null {
  const v = value.trim()
  if (!v) return null
  if (utf8ByteLength(v) > AVATAR_URL_MAX_LENGTH) return 'errors.urlTooLong'
  let url: URL | null = null
  try {
    url = new URL(v)
  } catch {
  }
  if (!url || (url.protocol !== 'https:' && url.protocol !== 'http:')) return 'errors.invalidUrl'
  return null
}
