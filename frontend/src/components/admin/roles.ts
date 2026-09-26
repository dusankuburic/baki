import type {TFunction} from 'i18next'
import type {RoleFilter, UserRole} from '@/api/admin'

// The roles an admin can assign, in the order the select offers them.
export const ROLE_OPTIONS: readonly UserRole[] = ['admin', 'member', 'viewer', 'guest']

// The role filter chips: every role, the four roles, then the accounts
// without a valid one.
export const ROLE_FILTERS: readonly RoleFilter[] = ['', 'admin', 'member', 'viewer', 'guest', 'none']

// roleLabel names a stored role for display; '' (no valid role) reads as
// unassigned, and an unexpected value is shown as stored.
export function roleLabel(t: TFunction<'admin'>, role: string): string {
  switch (role) {
    case 'admin':
      return t('users.roles.admin')
    case 'member':
      return t('users.roles.member')
    case 'viewer':
      return t('users.roles.viewer')
    case 'guest':
      return t('users.roles.guest')
    case '':
    case 'none':
      return t('users.roles.none')
    default:
      return role
  }
}
