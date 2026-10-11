import type {Organisation, OrgRole} from '@/stores/orgStore'

export const ROLES: OrgRole[] = ['admin', 'member', 'viewer', 'guest']

export const ROLE_LABEL_KEYS = {
  admin: 'orgs.roleAdmin',
  member: 'orgs.roleMember',
  viewer: 'orgs.roleViewer',
  guest: 'orgs.roleGuest',
} as const satisfies Record<OrgRole, string>

export function roleLabelKey(role: string): (typeof ROLE_LABEL_KEYS)[OrgRole] | null {
  return Object.prototype.hasOwnProperty.call(ROLE_LABEL_KEYS, role) ? ROLE_LABEL_KEYS[role as OrgRole] : null
}

export function orgNameId(orgId: string): string {
  return `org-name-${orgId}`
}

export function canUseOrgModels(org: Organisation, userId: string | undefined): boolean {
  const role = (org.members ?? []).find(m => m.userId === userId)?.role
  return role === 'admin' || role === 'member'
}

export function inviteLinkURL(token: string): string {
  const base = (import.meta.env.BASE_URL || '/').replace(/\/?$/, '/')
  return `${window.location.origin}${base}#invite=${encodeURIComponent(token)}`
}
