import {describe, it, expect, afterEach} from 'vitest'
import i18next, {setLanguage, LANGUAGE_KEY} from '@/i18n'

describe('i18n scaffold', () => {
  afterEach(() => {
    localStorage.removeItem(LANGUAGE_KEY)
    void i18next.changeLanguage('en')
  })

  it('initializes synchronously with bundled English resources', () => {
    expect(i18next.isInitialized).toBe(true)
    expect(i18next.t('findings:search.placeholder')).toBe('Search findings...')
  })

  it('resolves namespaced keys with interpolation', () => {
    expect(i18next.t('shell:toasts.exportedTo', {path: '/tmp/x.md'})).toBe('Exported to /tmp/x.md')
    expect(i18next.t('findings:summary.healthAria', {score: 85, label: 'Good'})).toBe('Health score 85 of 100: Good')
  })

  it('pluralizes counts via _one/_other', () => {
    expect(i18next.t('findings:summary.count', {count: 1})).toBe('1 finding')
    expect(i18next.t('findings:summary.count', {count: 5})).toBe('5 findings')
  })

  it('setLanguage persists the choice and updates the active language', () => {
    setLanguage('en')
    expect(localStorage.getItem(LANGUAGE_KEY)).toBe('en')
    expect(i18next.language).toBe('en')
  })

  it('keeps <html lang> in sync on language change', async () => {
    await i18next.changeLanguage('en')
    expect(document.documentElement.lang).toBe('en')
  })

  it('falls back to the key path (not a blank) for missing keys', () => {
    expect(i18next.t('findings:nonexistent.key' as never)).toBe('nonexistent.key')
  })
})
