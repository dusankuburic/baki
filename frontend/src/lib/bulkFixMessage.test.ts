import {describe, it, expect} from 'vitest'
import {bulkFixMessage} from './bulkFixMessage'

describe('bulkFixMessage', () => {
  it('reports a complete batch as success', () => {
    const m = bulkFixMessage(3, 2, false)
    expect(m.variant).toBe('success')
    expect(m.description).toBe('3 fix(es) across 2 rule(s).')
  })

  it('tells the user a partial batch was saved and can be continued', () => {
    const m = bulkFixMessage(2, 1, true)
    expect(m.variant).toBe('warning')
    expect(m.title).toMatch(/time budget/)
    expect(m.description).toMatch(/2 fix\(es\).*saved.*run Fix again/)
  })
})
