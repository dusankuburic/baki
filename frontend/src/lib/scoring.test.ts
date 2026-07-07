import {describe, it, expect} from 'vitest'
import {scoreColor, scoreBg, scoreLabel} from './scoring'

describe('score thresholds (80/60/40 boundaries)', () => {
  it('classifies 80+ as good (green)', () => {
    expect(scoreColor(80)).toBe('text-green-400')
    expect(scoreLabel(80)).toBe('Good')
    expect(scoreBg(100)).toBe('bg-green-500/10 border-green-500/20')
  })

  it('classifies 60-79 as fair (amber)', () => {
    expect(scoreColor(60)).toBe('text-amber-400')
    expect(scoreColor(79)).toBe('text-amber-400')
    expect(scoreLabel(65)).toBe('Fair')
  })

  it('classifies 40-59 as poor (orange)', () => {
    expect(scoreColor(40)).toBe('text-orange-400')
    expect(scoreColor(59)).toBe('text-orange-400')
    expect(scoreLabel(45)).toBe('Poor')
  })

  it('classifies below 40 as critical (red)', () => {
    expect(scoreColor(39)).toBe('text-red-400')
    expect(scoreColor(0)).toBe('text-red-400')
    expect(scoreLabel(0)).toBe('Critical')
    expect(scoreBg(-5)).toBe('bg-red-500/10 border-red-500/20')
  })
})
