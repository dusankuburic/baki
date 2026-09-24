// The message a signed-out user needs to see on the sign-in screen after an
// action that ends their session on purpose: a password change (the server
// revokes every refresh token, this tab's included) or account deletion.
// A toast cannot carry it — the toast host unmounts with the app shell at
// logout — so it rides in sessionStorage, which survives the unmount and is
// scoped to this tab, and LoginForm shows it once on mount.
const KEY = 'auth_notice'

export type PostLogoutNotice = 'password-changed' | 'account-deleted'

export function setPostLogoutNotice(notice: PostLogoutNotice): void {
  try {
    sessionStorage.setItem(KEY, notice)
  } catch {
    // Storage unavailable (private mode): the sign-in screen just has no note.
  }
}

export function readPostLogoutNotice(): PostLogoutNotice | null {
  try {
    const value = sessionStorage.getItem(KEY)
    return value === 'password-changed' || value === 'account-deleted' ? value : null
  } catch {
    return null
  }
}

export function clearPostLogoutNotice(): void {
  try {
    sessionStorage.removeItem(KEY)
  } catch {
    // Nothing stored, nothing to clear.
  }
}
