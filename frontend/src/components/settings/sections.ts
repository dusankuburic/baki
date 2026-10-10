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

// The Settings registry: which sections exist, how they group in the sidebar,
// which are cloud-only, and how each panel loads. The modal's nav, header,
// search and tests are all derived from it, so adding a section is one entry
// in SETTINGS_SECTIONS, its id in a group below, a `modal.sections.<id>`
// block in en.settings.ts, and the panel file. Record<SettingsSection, …>
// turns a new id in uiStore into a compile error until it is registered.

export type SettingsGroupId = 'application' | 'analysis' | 'ai' | 'workspace'

export interface SettingsGroup {
  id: SettingsGroupId
  sections: readonly SettingsSection[]
  // Cloud (multi-user) only: the desktop app is single-user with no orgs, so
  // the whole group disappears there.
  cloudOnly?: boolean
}

export interface SettingsSectionMeta {
  icon: LucideIcon
  cloudOnly?: boolean
  // Literal import() strings keep Vite's per-panel chunks.
  load: () => Promise<{default: ComponentType}>
}

export const SETTINGS_GROUPS: readonly SettingsGroup[] = [
  {id: 'application', sections: ['general', 'appearance', 'shortcuts', 'privacy']},
  {id: 'analysis', sections: ['parser', 'analysis', 'policies']},
  {id: 'ai', sections: ['accounts', 'behavior', 'prompts']},
  {id: 'workspace', sections: ['orgs', 'knowledge', 'tokens'], cloudOnly: true},
]

// Pinned at the bottom of the sidebar, under the groups.
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

// Each panel is lazy: only one is visible at a time, so the settings chunk
// fetches a panel on first open of that section instead of shipping all 14
// (including ~46 kB of cloud-only panels a desktop user can never open) with
// the modal. lazy() runs ONCE per section, at module level: a lazy created
// per render would remount (and refetch) the panel on every keystroke in the
// search box.
export const SETTINGS_PANELS = ALL_SECTION_IDS.reduce(
  (panels, id) => {
    panels[id] = lazy(SETTINGS_SECTIONS[id].load)
    return panels
  },
  {} as Record<SettingsSection, LazyExoticComponent<ComponentType>>,
)

// Organisations are a cloud-mode concept. Evaluated at render, not at module
// init, so tests can toggle the platform.
export const isCloudRuntime = (): boolean => !isTauri()

export function isSectionAvailable(id: SettingsSection, cloud: boolean): boolean {
  return cloud || !SETTINGS_SECTIONS[id].cloudOnly
}

// The groups with cloud-only ids stripped, and groups that end up empty dropped.
export function visibleGroups(cloud: boolean): SettingsGroup[] {
  return SETTINGS_GROUPS.filter(g => cloud || !g.cloudOnly)
    .map(g => ({...g, sections: g.sections.filter(id => isSectionAvailable(id, cloud))}))
    .filter(g => g.sections.length > 0)
}

export function visibleFooterSections(cloud: boolean): SettingsSection[] {
  return SETTINGS_FOOTER_SECTIONS.filter(id => isSectionAvailable(id, cloud))
}

// Every visible id in sidebar order: the groups, then the footer.
export function visibleSectionIds(cloud: boolean): SettingsSection[] {
  return [...visibleGroups(cloud).flatMap(g => g.sections), ...visibleFooterSections(cloud)]
}

// Search: every whitespace-separated token of the query has to occur in the
// section's haystack (label, description, group label and keywords), in any
// order and case. The matcher lives in lib/textMatch so other lists can
// filter the same way; it is re-exported for the nav and the tests.
export {matchesQuery} from '@/lib/textMatch'
