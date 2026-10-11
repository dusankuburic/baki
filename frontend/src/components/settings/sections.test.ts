import {describe, it, expect} from 'vitest'
import {enSettings} from '@/i18n/en.settings'
import {
  ALL_SECTION_IDS,
  SETTINGS_FOOTER_SECTIONS,
  SETTINGS_GROUPS,
  SETTINGS_SECTIONS,
  matchesQuery,
  visibleGroups,
  visibleSectionIds,
} from './sections'

describe('settings registry', () => {
  it('places every section exactly once in the sidebar', () => {
    const placed = [...SETTINGS_GROUPS.flatMap(g => g.sections), ...SETTINGS_FOOTER_SECTIONS]
    expect([...placed].sort()).toEqual([...ALL_SECTION_IDS].sort())
    expect(new Set(placed).size).toBe(placed.length)
  })

  it('has label, description and keywords copy for every section and group', () => {
    for (const id of ALL_SECTION_IDS) {
      const copy = enSettings.modal.sections[id]
      expect(copy.label, id).toBeTruthy()
      expect(copy.description, id).toBeTruthy()
      expect(copy.keywords, id).toBeTruthy()
    }
    for (const group of SETTINGS_GROUPS) {
      expect(enSettings.modal.groups[group.id], group.id).toBeTruthy()
    }
  })

  it('keeps the labels the e2e suite clicks on', () => {
    expect(enSettings.modal.sections.appearance.label).toBe('Appearance')
    expect(enSettings.modal.sections.about.label).toBe('About')
    expect(enSettings.modal.sections.shortcuts.label).toBe('Shortcuts')
  })

  it('hides the cloud-only sections on the desktop app', () => {
    const cloudOnly = ALL_SECTION_IDS.filter(id => SETTINGS_SECTIONS[id].cloudOnly).sort()
    expect(cloudOnly).toEqual(['knowledge', 'orgs', 'policies', 'tokens'])

    const desktop = visibleSectionIds(false)
    for (const id of cloudOnly) expect(desktop).not.toContain(id)
    expect(visibleGroups(false).map(g => g.id)).toEqual(['application', 'analysis', 'ai'])
    expect(desktop[desktop.length - 1]).toBe('about')

    expect(visibleSectionIds(true)).toEqual([...SETTINGS_GROUPS.flatMap(g => g.sections), ...SETTINGS_FOOTER_SECTIONS])
  })

  it('matches every query token, in any order and case', () => {
    const haystack = 'Appearance Customize the look and feel Application theme dark light'
    expect(matchesQuery(haystack, 'THEME')).toBe(true)
    expect(matchesQuery(haystack, 'dark theme')).toBe(true)
    expect(matchesQuery(haystack, 'dark tokens')).toBe(false)
    expect(matchesQuery(haystack, '   ')).toBe(true)
  })
})
