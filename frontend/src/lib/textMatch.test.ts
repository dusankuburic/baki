import {describe, it, expect} from 'vitest'
import {matchesQuery} from './textMatch'
import {matchesQuery as settingsMatchesQuery} from '@/components/settings/sections'

describe('matchesQuery', () => {
  const haystack = 'Appearance Customize the look and feel Application theme dark light'

  it('matches every query token, in any order and case', () => {
    expect(matchesQuery(haystack, 'THEME')).toBe(true)
    expect(matchesQuery(haystack, 'dark theme')).toBe(true)
    expect(matchesQuery(haystack, 'light   Dark')).toBe(true)
    expect(matchesQuery(haystack, 'dark tokens')).toBe(false)
  })

  it('treats an empty or blank query as matching everything', () => {
    expect(matchesQuery(haystack, '')).toBe(true)
    expect(matchesQuery(haystack, '   ')).toBe(true)
    expect(matchesQuery('', '')).toBe(true)
  })

  it('matches substrings, not only whole words', () => {
    expect(matchesQuery('Azure AI Foundry', 'found')).toBe(true)
    expect(matchesQuery('Azure AI Foundry', 'zur ai')).toBe(true)
  })

  it('is the matcher the Settings registry exports', () => {
    expect(settingsMatchesQuery).toBe(matchesQuery)
  })
})
