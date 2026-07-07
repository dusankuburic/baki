import {describe, it, expect} from 'vitest'
import {THEME_REGISTRY, DARK_THEMES, LIGHT_THEMES, SYSTEM_THEME} from './themeRegistry'

describe('THEME_REGISTRY', () => {
  it('has no duplicate theme ids', () => {
    const ids = THEME_REGISTRY.map(t => t.id)
    expect(new Set(ids).size).toBe(ids.length)
  })

  it('every entry is categorized as dark or light (never system)', () => {
    for (const t of THEME_REGISTRY) {
      expect(['dark', 'light']).toContain(t.category)
    }
  })
})

describe('DARK_THEMES / LIGHT_THEMES', () => {
  it('partition the registry with no overlap and no gaps', () => {
    expect(DARK_THEMES.length + LIGHT_THEMES.length).toBe(THEME_REGISTRY.length)
    expect(DARK_THEMES.every(t => t.category === 'dark')).toBe(true)
    expect(LIGHT_THEMES.every(t => t.category === 'light')).toBe(true)
  })

  it('includes the built-in "dark" and "light" defaults', () => {
    expect(DARK_THEMES.some(t => t.id === 'dark')).toBe(true)
    expect(LIGHT_THEMES.some(t => t.id === 'light')).toBe(true)
  })
})

describe('SYSTEM_THEME', () => {
  it('is not part of THEME_REGISTRY', () => {
    expect(THEME_REGISTRY.some(t => t.id === SYSTEM_THEME.id)).toBe(false)
  })
})
