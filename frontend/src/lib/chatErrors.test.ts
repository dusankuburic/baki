import {describe, it, expect} from 'vitest'
import {
  classifyChatFailure,
  budgetDetail,
  providerErrorInfo,
  isStreamGone,
  retryAfterHint,
  serverRestartDelayMs,
} from './chatErrors'

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

  // A deploy: the draining server ends the stream (or refuses its create)
  // with SERVER_RESTARTING, and the engine sends the turn again once.
  it('classifies SERVER_RESTARTING by its code', () => {
    expect(classifyChatFailure('the server is restarting', 'SERVER_RESTARTING')).toBe('serverRestarting')
    expect(classifyChatFailure('the server is restarting')).toBe('generic')
    expect(classifyChatFailure('lost', 'STREAM_INTERRUPTED')).toBe('generic')
  })
})

describe('isStreamGone', () => {
  // What /api/chat/resume answers for a stream this server does not know:
  // 403 under JWT (an unknown stream is "not yours"), 404 without.
  it('is true for 403, 404 and 410', () => {
    for (const status of [403, 404, 410]) expect(isStreamGone({status})).toBe(true)
  })

  it('is false for transient failures and non-errors', () => {
    for (const status of [0, 401, 429, 500, 502, 503]) expect(isStreamGone({status})).toBe(false)
    expect(isStreamGone(new Error('network'))).toBe(false)
    expect(isStreamGone(undefined)).toBe(false)
    expect(isStreamGone('404')).toBe(false)
  })
})

describe('retryAfterHint', () => {
  it("reads the envelope's details.retryAfterSeconds", () => {
    expect(retryAfterHint({status: 503, details: {retryAfterSeconds: 5}})).toBe(5)
  })

  it('is undefined without a usable hint', () => {
    expect(retryAfterHint({status: 503})).toBeUndefined()
    expect(retryAfterHint({details: {retryAfterSeconds: '5'}})).toBeUndefined()
    expect(retryAfterHint({details: {retryAfterSeconds: -1}})).toBeUndefined()
    expect(retryAfterHint({details: {retryAfterSeconds: Number.NaN}})).toBeUndefined()
    expect(retryAfterHint(null)).toBeUndefined()
  })
})

describe('serverRestartDelayMs', () => {
  it('waits the default 2 s plus up to 1 s of jitter without a hint', () => {
    expect(serverRestartDelayMs(undefined, () => 0)).toBe(2_000)
    expect(serverRestartDelayMs(undefined, () => 0.999)).toBe(2_999)
  })

  it("honours the server's Retry-After, capped", () => {
    expect(serverRestartDelayMs(5, () => 0)).toBe(5_000)
    expect(serverRestartDelayMs(3_600, () => 0)).toBe(10_000)
    expect(serverRestartDelayMs(0, () => 0)).toBe(2_000)
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

  it('maps PROVIDER_NOT_ALLOWED (the deployment or org policy) to "Choose another model" and a relist', () => {
    expect(providerErrorInfo('PROVIDER_NOT_ALLOWED')).toEqual({
      titleKey: 'errors.providerNotAllowedTitle',
      settingsSection: 'accounts',
      refreshProviders: true,
      action: 'chooseModel',
    })
  })

  it('is null for other or missing codes', () => {
    expect(providerErrorInfo('AI_BUDGET_EXCEEDED')).toBeNull()
    expect(providerErrorInfo('PROVIDER_RATE_LIMITED')).toBeNull()
    expect(providerErrorInfo('toString')).toBeNull()
    expect(providerErrorInfo(undefined)).toBeNull()
    expect(providerErrorInfo(null)).toBeNull()
  })
})

// PROVIDER_UNKNOWN (Phase 4b): the thread's provider is not in this server's
// registry — a newer bundle's pin, or a provider the deployment dropped.
describe('providerErrorInfo: PROVIDER_UNKNOWN', () => {
  it('maps it to a title, a listing refresh and "Choose another model"', () => {
    expect(providerErrorInfo('PROVIDER_UNKNOWN')).toEqual({
      titleKey: 'errors.providerUnknownTitle',
      settingsSection: 'accounts',
      refreshProviders: true,
      action: 'chooseModel',
    })
  })
})
