import {describe, it, expect} from 'vitest'
import {healthTier, scoreColor, scoreBg, scoreLabel} from './scoring'

describe('score thresholds (80/60/40 boundaries)', () => {
  it('classifies 80+ as good on the success token', () => {
    expect(healthTier(80).tier).toBe('good')
    expect(scoreColor(80)).toBe('text-semantic-success')
    expect(scoreLabel(80)).toBe('Good')
    expect(scoreBg(100)).toBe('bg-semantic-success/10 border-semantic-success/20')
    expect(healthTier(100).token).toBe('--success')
  })

  it('classifies 60-79 as fair on the warning token', () => {
    expect(scoreColor(60)).toBe('text-semantic-warning')
    expect(scoreColor(79)).toBe('text-semantic-warning')
    expect(scoreLabel(65)).toBe('Fair')
    expect(healthTier(65).fill).toBe('bg-semantic-warning')
  })

  it('classifies 40-59 as poor on the error token', () => {
    expect(healthTier(40).tier).toBe('poor')
    expect(scoreColor(59)).toBe('text-semantic-error')
    expect(scoreLabel(45)).toBe('Poor')
  })

  it('classifies below 40 as critical, still on the error token', () => {
    expect(healthTier(39).tier).toBe('critical')
    expect(scoreColor(0)).toBe('text-semantic-error')
    expect(scoreLabel(0)).toBe('Critical')
    expect(scoreBg(-5)).toBe('bg-semantic-error/10 border-semantic-error/20')
  })

  it('every class it returns is a literal Tailwind generates (no string-built names)', () => {
    for (const score of [95, 70, 50, 10]) {
      const t = healthTier(score)
      expect(t.fill).toMatch(/^bg-semantic-(success|warning|error)$/)
      expect(t.text).toMatch(/^text-semantic-(success|warning|error)$/)
    }
  })
})
