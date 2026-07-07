import {describe, it, expect} from 'vitest'
import {roleBadgeClass} from './roleBadge'

describe('roleBadgeClass', () => {
  it('returns the brand color for admin', () => {
    expect(roleBadgeClass('admin')).toBe('bg-brand-500/10 text-brand-400')
  })

  it('returns the info color for member', () => {
    expect(roleBadgeClass('member')).toBe('bg-semantic-info/10 text-semantic-info')
  })

  it('returns a neutral color for viewer', () => {
    expect(roleBadgeClass('viewer')).toBe('bg-surface-4 text-text-secondary')
  })

  it('falls back to a tertiary neutral for unknown roles', () => {
    expect(roleBadgeClass('superadmin')).toBe('bg-surface-4 text-text-tertiary')
  })
})
