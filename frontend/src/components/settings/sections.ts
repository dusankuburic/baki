import {lazy, type ComponentType, type LazyExoticComponent} from 'react'
import type {LucideIcon} from 'lucide-react'
import {
  BookOpen,
  Bot,
  Building2,
  FileCode2,
  Gauge,
  Info,
  Key,
  Keyboard,
  KeyRound,
  ListChecks,
  MessageSquareText,
  Palette,
  ShieldCheck,
  SlidersHorizontal,
} from 'lucide-react'
import {isTauri} from '@/platform/guards'
import type {SettingsSection} from '@/stores/uiStore'

export type SettingsGroupId = 'application' | 'analysis' | 'ai' | 'workspace'

export interface SettingsGroup {
  id: SettingsGroupId
  sections: readonly SettingsSection[]
  cloudOnly?: boolean
}

export interface SettingsSectionMeta {
  icon: LucideIcon
  cloudOnly?: boolean
  load: () => Promise<{default: ComponentType}>
}

export const SETTINGS_GROUPS: readonly SettingsGroup[] = [
  {id: 'application', sections: ['general', 'appearance', 'shortcuts', 'privacy']},
  {id: 'analysis', sections: ['parser', 'analysis', 'policies']},
  {id: 'ai', sections: ['accounts', 'behavior', 'prompts']},
  {id: 'workspace', sections: ['orgs', 'knowledge', 'tokens'], cloudOnly: true},
]

export const SETTINGS_FOOTER_SECTIONS: readonly SettingsSection[] = ['about']

export const SETTINGS_SECTIONS: Record<SettingsSection, SettingsSectionMeta> = {
  general: {icon: SlidersHorizontal, load: () => import('./GeneralPanel')},
  appearance: {icon: Palette, load: () => import('./AppearancePanel')},
  shortcuts: {icon: Keyboard, load: () => import('./ShortcutsPanel')},
  privacy: {icon: ShieldCheck, load: () => import('./PrivacyPanel')},
  parser: {icon: FileCode2, load: () => import('./ParserPanel')},
  analysis: {icon: ListChecks, load: () => import('./RulesPanel')},
  policies: {icon: Gauge, cloudOnly: true, load: () => import('./PolicyGatePanel')},
  accounts: {icon: KeyRound, load: () => import('./providers/ProvidersPanel')},
  behavior: {icon: Bot, load: () => import('./AIBehaviorPanel')},
  prompts: {icon: MessageSquareText, load: () => import('./AIPromptsPanel')},
  orgs: {icon: Building2, cloudOnly: true, load: () => import('./OrganizationsPanel')},
  knowledge: {icon: BookOpen, cloudOnly: true, load: () => import('./KnowledgeBasePanel')},
  tokens: {icon: Key, cloudOnly: true, load: () => import('./ApiTokensPanel')},
  about: {icon: Info, load: () => import('./AboutPanel')},
}

export const ALL_SECTION_IDS = Object.keys(SETTINGS_SECTIONS) as SettingsSection[]

export const SETTINGS_PANELS = ALL_SECTION_IDS.reduce(
  (panels, id) => {
    panels[id] = lazy(SETTINGS_SECTIONS[id].load)
    return panels
  },
  {} as Record<SettingsSection, LazyExoticComponent<ComponentType>>,
)

export const isCloudRuntime = (): boolean => !isTauri()

export function isSectionAvailable(id: SettingsSection, cloud: boolean): boolean {
  return cloud || !SETTINGS_SECTIONS[id].cloudOnly
}

export function visibleGroups(cloud: boolean): SettingsGroup[] {
  return SETTINGS_GROUPS.filter(g => cloud || !g.cloudOnly)
    .map(g => ({...g, sections: g.sections.filter(id => isSectionAvailable(id, cloud))}))
    .filter(g => g.sections.length > 0)
}

export function visibleFooterSections(cloud: boolean): SettingsSection[] {
  return SETTINGS_FOOTER_SECTIONS.filter(id => isSectionAvailable(id, cloud))
}

export function visibleSectionIds(cloud: boolean): SettingsSection[] {
  return [...visibleGroups(cloud).flatMap(g => g.sections), ...visibleFooterSections(cloud)]
}

export {matchesQuery} from '@/lib/textMatch'
