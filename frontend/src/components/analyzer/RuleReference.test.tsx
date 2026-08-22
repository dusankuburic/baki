import {describe, it, expect, vi, beforeEach} from 'vitest'
import {render, screen, fireEvent, waitFor} from '@testing-library/react'
import type {Rule} from '@/types/analysis'

const mockRules: Rule[] = [
  {
    id: 'unhandled-error',
    name: 'Unhandled error',
    description: 'Fallible action without error handler',
    defaultSeverity: 'warning',
    category: 'Reliability',
    enabled: true,
    confidence: 'high',
    autoFix: 'wrap-error-handler',
  },
  {
    id: 'hardcoded-credential',
    name: 'Hardcoded credential',
    description: 'Secret literal in property',
    defaultSeverity: 'error',
    category: 'Security',
    enabled: true,
    confidence: 'high',
    autoFix: 'replace-with-variable',
  },
  {
    id: 'deep-nesting',
    name: 'Deep nesting',
    description: 'Block nested too deeply',
    defaultSeverity: 'info',
    category: 'Style',
    enabled: true,
    confidence: 'low',
  },
  {
    id: 'duplicate-label',
    name: 'Duplicate label',
    description: 'Two LABEL blocks with same name',
    defaultSeverity: 'warning',
    category: 'Logic',
    enabled: true,
    confidence: 'high',
  },
]

const getRules = vi.fn()

vi.mock('@/api/analysis', () => ({
  analysisApi: {
    getRules: (...a: unknown[]) => getRules(...a),
  },
}))

import RuleReference from './RuleReference'

describe('RuleReference', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    getRules.mockResolvedValue(mockRules)
  })

  it('renders all rules on load', async () => {
    render(<RuleReference />)
    await waitFor(() => {
      expect(screen.getByText('unhandled-error')).toBeInTheDocument()
      expect(screen.getByText('hardcoded-credential')).toBeInTheDocument()
      expect(screen.getByText('deep-nesting')).toBeInTheDocument()
    })
    // Summary line: 4 rules, 2 with auto-fix
    expect(screen.getByText(/4 rules/)).toBeInTheDocument()
    expect(screen.getByText(/2 with auto-fix/)).toBeInTheDocument()
  })

  it('filters by search query (rule id)', async () => {
    render(<RuleReference />)
    await waitFor(() => expect(screen.getByText('unhandled-error')).toBeInTheDocument())

    const input = screen.getByPlaceholderText('Search rules...')
    fireEvent.change(input, {target: {value: 'hardcoded'}})

    expect(screen.getByText('hardcoded-credential')).toBeInTheDocument()
    expect(screen.queryByText('unhandled-error')).not.toBeInTheDocument()
  })

  it('filters by search query (rule name)', async () => {
    render(<RuleReference />)
    await waitFor(() => expect(screen.getByText('unhandled-error')).toBeInTheDocument())

    const input = screen.getByPlaceholderText('Search rules...')
    fireEvent.change(input, {target: {value: 'deep nesting'}})

    expect(screen.getByText('deep-nesting')).toBeInTheDocument()
    expect(screen.queryByText('hardcoded-credential')).not.toBeInTheDocument()
  })

  it('shows auto-fix badge on rules with fixers', async () => {
    render(<RuleReference />)
    await waitFor(() => expect(screen.getByText('unhandled-error')).toBeInTheDocument())

    // unhandled-error has auto-fix
    expect(screen.getAllByText('auto-fix').length).toBeGreaterThanOrEqual(1)
    // deep-nesting has no auto-fix — its card shouldn't show the badge
    // (we check by counting: 2 rules have autoFix, so 2 badges)
    expect(screen.getAllByText('auto-fix')).toHaveLength(2)
  })

  it('shows empty state when search matches nothing', async () => {
    render(<RuleReference />)
    await waitFor(() => expect(screen.getByText('unhandled-error')).toBeInTheDocument())

    const input = screen.getByPlaceholderText('Search rules...')
    fireEvent.change(input, {target: {value: 'nonexistent-rule'}})

    expect(screen.getByText(/No rules match/)).toBeInTheDocument()
  })

  it('shows error state when API fails', async () => {
    getRules.mockRejectedValue(new Error('network down'))
    render(<RuleReference />)
    await waitFor(() => {
      expect(screen.getByText('Failed to load rules.')).toBeInTheDocument()
    })
  })
})
