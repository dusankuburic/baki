import {describe, it, expect} from 'vitest'
import {classifyChatFailure, budgetDetail, providerErrorInfo} from './chatErrors'

describe('classifyChatFailure', () => {
  it('trusts the machine-readable code first', () => {
    expect(classifyChatFailure('anything', 'AI_BUDGET_EXCEEDED')).toBe('budget')
    expect(classifyChatFailure('anything', 'CHAT_CAPACITY_REACHED')).toBe('capacity')
    // A coded non-budget failure is not second-guessed from its wording.
    expect(classifyChatFailure('budget exceeded', 'PROVIDER_UNAVAILABLE')).toBe('generic')
  })

  it('falls back to the message for backends that send no code', () => {
    expect(classifyChatFailure('daily AI budget exceeded ($1.02 / $1.00)')).toBe('budget')
    expect(classifyChatFailure('per-user daily AI budget exceeded ($2 / $1)')).toBe('budget')
    expect(classifyChatFailure('too many chat responses running at once')).toBe('capacity')
  })

  it('does not treat a failed budget CHECK as the budget being reached', () => {
    expect(classifyChatFailure("couldn't verify today's AI budget — try again in a moment")).toBe('generic')
  })

  // The create request refuses a model the picker listed but the server no
  // longer offers: the engine returns the message to the composer for it.
  it('classifies MODEL_NOT_AVAILABLE by its code', () => {
    expect(classifyChatFailure('model not available', 'MODEL_NOT_AVAILABLE')).toBe('modelNotAvailable')
    expect(classifyChatFailure('model not available')).toBe('generic')
  })
})

describe('budgetDetail', () => {
  it('extracts the spent / limit figures', () => {
    expect(budgetDetail('daily AI budget exceeded ($1.02 / $1.00, of which $0.00 is in flight)')).toBe(
      ' ($1.02 / $1.00)',
    )
  })

  it('is empty when there are no figures', () => {
    expect(budgetDetail('budget exceeded')).toBe('')
  })
})

describe('providerErrorInfo', () => {
  it('maps the provider codes to a title, a settings section and a refresh flag', () => {
    expect(providerErrorInfo('PROVIDER_AUTH')).toEqual({
      titleKey: 'errors.providerAuthTitle',
      settingsSection: 'accounts',
      refreshProviders: true,
    })
    expect(providerErrorInfo('PROVIDER_NOT_CONFIGURED')?.refreshProviders).toBe(true)
    expect(providerErrorInfo('PROVIDER_PERMISSION')?.refreshProviders).toBe(false)
    // The Max tokens fix lives in AI Behavior.
    expect(providerErrorInfo('PROVIDER_OUTPUT_LIMIT')?.settingsSection).toBe('behavior')
    for (const code of ['PROVIDER_UNAVAILABLE', 'PROVIDER_CONTENT_FILTERED', 'PROVIDER_BALANCE']) {
      expect(providerErrorInfo(code)).not.toBeNull()
    }
  })

  it('maps MODEL_NOT_AVAILABLE to "Choose another model" and a listing refresh', () => {
    expect(providerErrorInfo('MODEL_NOT_AVAILABLE')).toEqual({
      titleKey: 'errors.modelNotAvailable',
      settingsSection: 'accounts',
      refreshProviders: true,
      action: 'chooseModel',
    })
    // The settings-fixable codes keep "Open settings" (no action).
    expect(providerErrorInfo('PROVIDER_AUTH')?.action).toBeUndefined()
  })

  it('is null for other or missing codes', () => {
    expect(providerErrorInfo('AI_BUDGET_EXCEEDED')).toBeNull()
    expect(providerErrorInfo('PROVIDER_RATE_LIMITED')).toBeNull()
    expect(providerErrorInfo('toString')).toBeNull()
    expect(providerErrorInfo(undefined)).toBeNull()
    expect(providerErrorInfo(null)).toBeNull()
  })
})
