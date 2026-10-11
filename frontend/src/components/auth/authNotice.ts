import {logger} from '@/lib/logger'

const KEY = 'auth_notice'

export type PostLogoutNotice = 'password-changed' | 'account-deleted'

export function setPostLogoutNotice(notice: PostLogoutNotice): void {
  try {
    sessionStorage.setItem(KEY, notice)
  } catch (err) {
    logger.warn('setPostLogoutNotice: sessionStorage unavailable', err)
  }
}

export function readPostLogoutNotice(): PostLogoutNotice | null {
  try {
    const value = sessionStorage.getItem(KEY)
    return value === 'password-changed' || value === 'account-deleted' ? value : null
  } catch (err) {
    logger.warn('readPostLogoutNotice: sessionStorage unavailable', err)
    return null
  }
}

export function clearPostLogoutNotice(): void {
  try {
    sessionStorage.removeItem(KEY)
  } catch (err) {
    logger.warn('clearPostLogoutNotice: sessionStorage unavailable', err)
  }
}
