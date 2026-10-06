import type {Organisation, OrgRole} from '@/stores/orgStore'

export const ROLES: OrgRole[] = ['admin', 'member', 'viewer', 'guest']

// The i18n key naming each role. `satisfies` keeps it in step with OrgRole.
export const ROLE_LABEL_KEYS = {
  admin: 'orgs.roleAdmin',
  member: 'orgs.roleMember',
  viewer: 'orgs.roleViewer',
  guest: 'orgs.roleGuest',
} as const satisfies Record<OrgRole, string>

// roleLabelKey tolerates a role this client does not know (a newer server):
// null, and the caller shows the raw value.
export function roleLabelKey(role: string): (typeof ROLE_LABEL_KEYS)[OrgRole] | null {
  return Object.prototype.hasOwnProperty.call(ROLE_LABEL_KEYS, role) ? ROLE_LABEL_KEYS[role as OrgRole] : null
}

// The organisation's name in its row: a deep link's landing spot when the
// organisation is not the one expanded.
export function orgNameId(orgId: string): string {
  return `org-name-${orgId}`
}

// Admins and members chat with the organisation's shared Azure AI Foundry
// models; viewers and guests may not (the server refuses them the listing).
export function canUseOrgModels(org: Organisation, userId: string | undefined): boolean {
  const role = (org.members ?? []).find(m => m.userId === userId)?.role
  return role === 'admin' || role === 'member'
}

// inviteLinkURL builds the same deep link the invite email carries
// (`<base>/#invite=<token>`, see internal/mail/service.go). Shown once after
// creating an invite so an admin can hand it over when email delivery is not
// configured — the server always returned the token, the UI threw it away.
export function inviteLinkURL(token: string): string {
  const base = (import.meta.env.BASE_URL || '/').replace(/\/?$/, '/')
  return `${window.location.origin}${base}#invite=${encodeURIComponent(token)}`
}
