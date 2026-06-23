// Account-recovery tokens arrive in the URL fragment placed there by links in
// transactional emails (see internal/mail/service.go):
//   #resetPassword=<token>  -> password reset
//   #verifyEmail=<token>    -> email verification
//   #invite=<token>         -> org invitation acceptance
//
// parseRecoveryHash is pure (safe to call from a render/useState initializer,
// which React StrictMode double-invokes); clearRecoveryHash performs the
// history mutation and must run from an effect, not during render.
export interface RecoveryHash {
  resetToken?: string
  verifyToken?: string
  inviteToken?: string
}

export function parseRecoveryHash(): RecoveryHash {
  const hash = window.location.hash.replace(/^#/, '')
  if (!hash.includes('resetPassword=') && !hash.includes('verifyEmail=') && !hash.includes('invite=')) return {}
  const params = new URLSearchParams(hash)
  return {
    resetToken: params.get('resetPassword') ?? undefined,
    verifyToken: params.get('verifyEmail') ?? undefined,
    inviteToken: params.get('invite') ?? undefined,
  }
}

// clearRecoveryHash strips the fragment so the single-use token doesn't linger
// in browser history. Call from an effect after the token has been captured.
export function clearRecoveryHash(): void {
  history.replaceState(null, '', window.location.pathname + window.location.search)
}
