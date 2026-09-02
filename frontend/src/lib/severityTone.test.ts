import {describe, it, expect} from 'vitest'
import {severityTone, triageTone, successActionTone} from './severityTone'

// The single source of severity/status tone classes (U2.1): semantic tokens
// only — a raw-palette class here means a theme desync regression.
describe('severityTone', () => {
  it('maps every severity to semantic-token classes', () => {
    for (const sev of ['error', 'warning', 'info'] as const) {
      const tone = severityTone(sev)
      for (const cls of [tone.text, tone.bg, tone.border, tone.dot, tone.bar]) {
        expect(cls).toMatch(/semantic-(error|warning|info|success)/)
      }
    }
  })

  it('error/warning/info map to their own semantic hue', () => {
    expect(severityTone('error').text).toBe('text-semantic-error')
    expect(severityTone('warning').text).toBe('text-semantic-warning')
    expect(severityTone('info').text).toBe('text-semantic-info')
  })

  it('unknown severities fall back to info, never crash', () => {
    expect(severityTone('nonsense' as never).text).toBe('text-semantic-info')
  })
})

describe('triageTone', () => {
  it('resolved reads as success, in_progress as warning, acknowledged as info', () => {
    expect(triageTone('resolved').text).toBe('text-semantic-success')
    expect(triageTone('in_progress').text).toBe('text-semantic-warning')
    expect(triageTone('acknowledged').text).toBe('text-semantic-info')
  })

  it('open/suppressed are muted surface tones, not hues', () => {
    expect(triageTone('open').text).toBe('text-text-secondary')
    expect(triageTone('suppressed').dot).toBe('bg-text-disabled')
  })

  it('every status resolves', () => {
    for (const st of ['open', 'acknowledged', 'in_progress', 'resolved', 'suppressed'] as const) {
      expect(triageTone(st).bar).toBeTruthy()
    }
  })
})

describe('successActionTone', () => {
  it('rides the success semantic token', () => {
    expect(successActionTone.text).toBe('text-semantic-success')
  })
})
