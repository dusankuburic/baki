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

export function clearRecoveryHash(): void {
  history.replaceState(null, '', window.location.pathname + window.location.search)
}
