import {describe, it, expect} from 'vitest'
import {PROVIDERS, PROVIDER_OPTIONS} from './providers'

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
    ])
    for (const o of PROVIDER_OPTIONS) {
      expect(o.label).toBe(PROVIDERS[o.value].label)
    }
  })
})
