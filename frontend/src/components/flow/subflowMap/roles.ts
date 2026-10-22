import type {SubflowRole} from './model'

/** i18n keys (namespace `flow`) for the short role badge on a map card. */
export type RoleBadgeKey =
  | 'graph.roleBadgeEntry'
  | 'graph.roleBadgeCoordinator'
  | 'graph.roleBadgeLeaf'
  | 'graph.roleBadgeIsolated'
  | 'graph.roleBadgeIntermediate'

export interface RoleConfig {
  badgeKey: RoleBadgeKey
  color: string
  bgLight: string
  bgDark: string
}

/** Colour and badge per subflow role, shared by the map's cards and the focus panel. */
export const ROLE_CONFIGS: Record<SubflowRole, RoleConfig> = {
  entry: {
    badgeKey: 'graph.roleBadgeEntry',
    color: '#3b82f6', // blue-500
    bgLight: '#eff6ff',
    bgDark: 'rgba(59, 130, 246, 0.15)',
  },
  coordinator: {
    badgeKey: 'graph.roleBadgeCoordinator',
    color: '#8b5cf6', // purple-500
    bgLight: '#f5f3ff',
    bgDark: 'rgba(139, 92, 246, 0.15)',
  },
  leaf: {
    badgeKey: 'graph.roleBadgeLeaf',
    color: '#10b981', // emerald-500
    bgLight: '#ecfdf5',
    bgDark: 'rgba(16, 185, 129, 0.15)',
  },
  isolated: {
    badgeKey: 'graph.roleBadgeIsolated',
    color: '#64748b', // slate-500
    bgLight: '#f8fafc',
    bgDark: 'rgba(100, 116, 139, 0.15)',
  },
  intermediate: {
    badgeKey: 'graph.roleBadgeIntermediate',
    color: '#0284c7', // sky-600
    bgLight: '#f0f9ff',
    bgDark: 'rgba(2, 132, 199, 0.15)',
  },
}

export function roleConfig(role: SubflowRole | undefined): RoleConfig {
  return (role && ROLE_CONFIGS[role]) || ROLE_CONFIGS.intermediate
}
