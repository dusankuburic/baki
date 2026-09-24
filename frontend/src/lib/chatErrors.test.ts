import {describe, it, expect} from 'vitest'
import {classifyChatFailure, budgetDetail} from './chatErrors'

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
