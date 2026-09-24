import {describe, it, expect} from 'vitest'
import {describeUserAgent, deviceKind} from './userAgent'

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

describe('deviceKind', () => {
  it('maps desktop, phone and tablet user agents to an icon kind', () => {
    expect(deviceKind('Mozilla/5.0 (Windows NT 10.0; Win64; x64) Chrome/120.0.0.0 Safari/537.36')).toBe('desktop')
    expect(deviceKind('Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) Version/17.0 Safari/605.1.15')).toBe('desktop')
    expect(deviceKind('Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) Mobile/15E148 Safari/604.1')).toBe(
      'mobile',
    )
    expect(deviceKind('Mozilla/5.0 (Linux; Android 14; Pixel 8) Chrome/120.0.0.0 Mobile Safari/537.36')).toBe('mobile')
    expect(deviceKind('Mozilla/5.0 (iPad; CPU OS 17_0 like Mac OS X) Version/17.0 Safari/605.1.15')).toBe('tablet')
    expect(deviceKind('Mozilla/5.0 (Linux; Android 14; SM-X910) Chrome/120.0.0.0 Safari/537.36')).toBe('tablet')
  })

  it('is unknown for an empty or unrecognised user agent', () => {
    expect(deviceKind(undefined)).toBe('unknown')
    expect(deviceKind('')).toBe('unknown')
    expect(deviceKind('SomeWeirdBot/1.0')).toBe('unknown')
  })
})
