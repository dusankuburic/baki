import type {TFunction} from 'i18next'
import type {RoleFilter, UserRole} from '@/api/admin'

export const ROLE_OPTIONS: readonly UserRole[] = ['admin', 'member', 'viewer', 'guest']

export const ROLE_FILTERS: readonly RoleFilter[] = ['', 'admin', 'member', 'viewer', 'guest', 'none']

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
