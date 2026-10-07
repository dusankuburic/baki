import {describe, it, expect, vi, beforeEach} from 'vitest'
import {render, screen} from '@testing-library/react'
import {useGovernanceStore} from '@/stores/governanceStore'
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
})
