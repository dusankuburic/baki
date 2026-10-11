import {describe, it, expect, expectTypeOf} from 'vitest'
import {PROVIDERS, PROVIDER_OPTIONS, isKnownProviderId} from './providers'
import type {KnownProviderID, ProviderID} from './providers'

describe('PROVIDER_OPTIONS', () => {
  it('offers every provider in the catalogue exactly once', () => {
    expect(PROVIDER_OPTIONS.map(o => o.value)).toEqual([
      'claude',
      'openai',
      'gemini',
      'xai',
      'glm',
      'github-models',
      'copilot',
      'azure-foundry',
      'local',
    ])
    for (const o of PROVIDER_OPTIONS) {
      expect(o.label).toBe(PROVIDERS[o.value].label)
    }
  })
})

describe('ProviderID', () => {
  it('a string the catalogue does not know is a ProviderID; isKnownProviderId narrows it', () => {
    const id: ProviderID = 'mistral'
    expectTypeOf(id).toEqualTypeOf<string>()
    expect(isKnownProviderId(id)).toBe(false)
    expect(isKnownProviderId('claude')).toBe(true)
    expect(isKnownProviderId(7)).toBe(false)
    expect(isKnownProviderId('toString')).toBe(false)
    const picked: ProviderID = 'claude'
    if (isKnownProviderId(picked)) {
      expectTypeOf(picked).toEqualTypeOf<KnownProviderID>()
      expect(PROVIDERS[picked].label).toBe('Claude')
    }
  })
})
