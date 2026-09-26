// Pure helpers for the audit log's action filter and badges.

// The groups the action filter is organised by, in display order; the ids
// are the prefixes the server uses ("auth.login" → auth).
export const AUDIT_GROUPS = [
  'auth',
  'admin',
  'flow',
  'finding',
  'chat',
  'settings',
  'keys',
  'org',
  'user',
  'token',
  'policy',
  'governance',
  'other',
] as const
export type AuditGroup = (typeof AUDIT_GROUPS)[number]

const KNOWN: ReadonlySet<string> = new Set(AUDIT_GROUPS)

// actionGroup: "auth.login" → auth; the four dotless ids ("org_channel_save",
// "org_rule_delete") → org; anything unrecognised → other.
export function actionGroup(action: string): AuditGroup {
  const head = action.includes('.') ? action.split('.')[0] : action.split('_')[0]
  return KNOWN.has(head) && head !== 'other' ? (head as AuditGroup) : 'other'
}

// groupActions sorts the ids and buckets them by group, keeping only the
// groups that have members, in AUDIT_GROUPS order.
export function groupActions(actions: readonly string[]): Array<{group: AuditGroup; actions: string[]}> {
  const by = new Map<AuditGroup, string[]>()
  for (const action of [...actions].sort()) {
    const group = actionGroup(action)
    by.set(group, [...(by.get(group) ?? []), action])
  }
  return AUDIT_GROUPS.filter(g => by.has(g)).map(g => ({group: g, actions: by.get(g) ?? []}))
}

// actionTone: failed sign-ins and lockouts stand out, administration wears
// the brand tint, everything else stays neutral — all on theme tokens.
export function actionTone(action: string): string {
  if (action === 'auth.login_failure' || action === 'auth.account_lock') {
    return 'bg-semantic-error/10 text-semantic-error'
  }
  if (action.startsWith('admin.')) return 'bg-brand-500/10 text-brand-400'
  return 'bg-surface-3 text-text-secondary'
}
