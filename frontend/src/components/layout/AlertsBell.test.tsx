import {describe, it, expect, vi, beforeEach} from 'vitest'
import {fireEvent, render, screen} from '@testing-library/react'
import {useGovernanceStore} from '@/stores/governanceStore'
import {useUIStore} from '@/stores/uiStore'
import {libraryApi} from '@/api/library'
import {unknownValue} from '@/testing/testHelpers'
import type {GovernanceAlert} from '@/api/governance'
import AlertsBell from './AlertsBell'

vi.mock('@/api/library', () => ({libraryApi: {getContent: vi.fn()}}))

const alert = (over: Partial<GovernanceAlert> = {}): GovernanceAlert => ({
  id: 'a1',
  flowId: 'f1',
  flowName: 'Invoices',
  type: 'drift',
  title: 'Drift detected',
  severity: 'warning',
  createdAt: '2026-01-01T00:00:00Z',
  ...over,
})

beforeEach(() => {
  useGovernanceStore.getState().reset()
  useUIStore.getState().closeSettings()
  vi.mocked(libraryApi.getContent).mockClear()
})

describe('AlertsBell', () => {
  // Tolerant wire enums: the server writes an alert's title and message, so
  // one of a type or severity this app does not know reads like any other.
  it('lists an alert of a type and severity a newer server added beside the known ones', () => {
    useGovernanceStore.setState({
      panelOpen: true,
      loading: false,
      alerts: [
        alert(),
        alert({
          id: 'a2',
          type: unknownValue('policy_failed'),
          severity: unknownValue('critical'),
          title: 'Policy gate failed',
          message: '3 new errors',
        }),
      ],
    })
    render(<AlertsBell />)
    expect(screen.getByText('Drift detected')).toBeInTheDocument()
    expect(screen.getByText('Policy gate failed')).toBeInTheDocument()
    expect(screen.getByText('3 new errors')).toBeInTheDocument()
    // Its chip carries the server's own word in the neutral warning style.
    expect(screen.getByText('critical')).toHaveClass('text-amber-400')
  })

  // A "new rules" notice is about rules, not one flow: the row lists the rules
  // and their reach, names the scope where an alert names its flow, and opens
  // the analysis rule settings instead of a flow.
  it('renders a new_rules notice and opens the analysis rules from it', () => {
    useGovernanceStore.setState({
      panelOpen: true,
      loading: false,
      alerts: [
        alert({
          id: 'new_rules|org|org-1|rs1:d',
          flowId: '',
          flowName: undefined,
          type: 'new_rules',
          title: '6 new rules report findings',
          message: 'rule-a, rule-b, …',
          ruleIds: ['rule-a', 'rule-b', 'rule-c', 'rule-d', 'rule-e', 'rule-f'],
          scope: 'org',
          flowCount: 3,
        }),
      ],
    })
    render(<AlertsBell />)
    expect(screen.getByText('rules')).toBeInTheDocument()
    expect(screen.getByText('rule-a')).toBeInTheDocument()
    expect(screen.getByText('rule-d')).toBeInTheDocument()
    expect(screen.queryByText('rule-e')).not.toBeInTheDocument()
    expect(screen.getByText('+2 more')).toBeInTheDocument()
    expect(screen.getByText(/Findings in 3 flows/)).toBeInTheDocument()
    expect(screen.getByText('Your organisation')).toBeInTheDocument()
    // The rule chips replace the server's sentence, which lists the same ids.
    expect(screen.queryByText('rule-a, rule-b, …')).not.toBeInTheDocument()

    fireEvent.click(screen.getByText('6 new rules report findings'))
    expect(useUIStore.getState().settingsOpen).toBe(true)
    expect(useUIStore.getState().settingsSection).toBe('analysis')
    expect(useGovernanceStore.getState().panelOpen).toBe(false)
    expect(libraryApi.getContent).not.toHaveBeenCalled()
  })

  it('opens the flow of a drift alert, not the settings', () => {
    vi.mocked(libraryApi.getContent).mockResolvedValue({} as never)
    useGovernanceStore.setState({panelOpen: true, loading: false, alerts: [alert()]})
    render(<AlertsBell />)
    expect(screen.getByText('Invoices')).toBeInTheDocument()
    fireEvent.click(screen.getByText('Drift detected'))
    expect(libraryApi.getContent).toHaveBeenCalledWith('f1')
    expect(useUIStore.getState().settingsOpen).toBe(false)
  })
})
