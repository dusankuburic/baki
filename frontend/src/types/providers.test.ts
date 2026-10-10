import {describe, it, expect, expectTypeOf} from 'vitest'
import {PROVIDERS, PROVIDER_OPTIONS, isKnownProviderId} from './providers'
import type {KnownProviderID, ProviderID} from './providers'

describe('PROVIDER_OPTIONS', () => {
  // The record's `satisfies Record<ProviderID, …>` is the compile-time half;
  // this pins the derived picker options to it, in declaration order.
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

// The server names providers (Phase 4b): a tab older than it still sends and
// shows their ids. Only a label, glyph or default needs the known set.
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
