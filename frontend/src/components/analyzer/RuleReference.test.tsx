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

    expect(screen.getAllByText('auto-fix').length).toBeGreaterThanOrEqual(1)
    expect(screen.getAllByText('auto-fix')).toHaveLength(2)
  })

  it('shows empty state when search matches nothing', async () => {
    render(<RuleReference />)
    await waitFor(() => expect(screen.getByText('unhandled-error')).toBeInTheDocument())

    const input = screen.getByPlaceholderText('Search rules...')
    fireEvent.change(input, {target: {value: 'nonexistent-rule'}})

    expect(screen.getByText(/No rules match/)).toBeInTheDocument()
  })

  it('marks preview rules and library rules, and counts the preview ones', async () => {
    getRules.mockResolvedValue([
      ...mockRules.map(r => ({...r, stage: 'stable', kind: 'block', introducedIn: 1})),
      {
        id: 'duplicate-subflow-across-flows',
        name: 'Duplicate subflow across flows',
        description: 'Same subflow body in two flows',
        defaultSeverity: 'info',
        category: 'Style',
        enabled: true,
        confidence: 'high',
        stage: 'preview',
        kind: 'library',
        introducedIn: 1,
      },
      {...mockRules[3]!, id: 'future-rule', stage: 'beta', kind: 'project'},
    ] as Rule[])
    render(<RuleReference />)
    await waitFor(() => expect(screen.getByText('duplicate-subflow-across-flows')).toBeInTheDocument())

    expect(screen.getAllByText('Preview')).toHaveLength(1)
    expect(screen.getByText(/1 in preview/)).toBeInTheDocument()
    expect(screen.getAllByText('Across flows')).toHaveLength(1)
    const card = screen.getByText('duplicate-subflow-across-flows').closest('div.p-4') as HTMLElement
    expect(card).toHaveTextContent('Preview')
    expect(card).toHaveTextContent('Across flows')
    expect(screen.getByText('future-rule')).toBeInTheDocument()
  })

  it('shows no preview count when no rule is in preview', async () => {
    render(<RuleReference />)
    await waitFor(() => expect(screen.getByText('unhandled-error')).toBeInTheDocument())
    expect(screen.queryByText(/in preview/)).not.toBeInTheDocument()
    expect(screen.queryByText('Preview')).not.toBeInTheDocument()
  })

  it('shows error state when API fails', async () => {
    getRules.mockRejectedValue(new Error('network down'))
    render(<RuleReference />)
    await waitFor(() => {
      expect(screen.getByText('Failed to load rules.')).toBeInTheDocument()
    })
  })
})
