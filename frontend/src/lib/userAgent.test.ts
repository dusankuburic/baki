import {describe, it, expect} from 'vitest'
import {describeUserAgent} from './userAgent'

describe('describeUserAgent', () => {
  it('returns Unknown device for empty/undefined input', () => {
    expect(describeUserAgent(undefined)).toBe('Unknown device')
    expect(describeUserAgent('')).toBe('Unknown device')
  })

  it('identifies Chrome on Windows', () => {
    const ua =
      'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36'
    expect(describeUserAgent(ua)).toBe('Chrome on Windows')
  })

  it('identifies Firefox on Linux', () => {
    const ua = 'Mozilla/5.0 (X11; Linux x86_64; rv:120.0) Gecko/20100101 Firefox/120.0'
    expect(describeUserAgent(ua)).toBe('Firefox on Linux')
  })

  it('identifies Safari on macOS (not misclassified as Chrome)', () => {
    const ua =
      'Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.0 Safari/605.1.15'
    expect(describeUserAgent(ua)).toBe('Safari on macOS')
  })

  it('identifies Edge on Windows (not misclassified as Chrome)', () => {
    const ua =
      'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36 Edg/120.0.0.0'
    expect(describeUserAgent(ua)).toBe('Edge on Windows')
  })

  it('identifies mobile Safari on iOS', () => {
    const ua =
      'Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.0 Mobile/15E148 Safari/604.1'
    expect(describeUserAgent(ua)).toBe('Safari on iOS')
  })

  it('falls back gracefully for an unrecognized UA', () => {
    expect(describeUserAgent('SomeWeirdBot/1.0')).toBe('Unknown device')
  })
})
