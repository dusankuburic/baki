import {describe, it, expect, vi, beforeEach} from 'vitest'
import {render, screen, fireEvent} from '@testing-library/react'
import {ToastProvider} from '@/components/shared/Toast'
import {ConfirmProvider} from '@/components/shared/ConfirmDialog'
import {useAnalysisStore, findingKey} from '@/stores/analysisStore'
import {useOrgStore, type Organisation} from '@/stores/orgStore'
import {unknownValue} from '@/testing/testHelpers'
import type {Finding, FindingStatus} from '@/types'
import FindingTriageMenu from './FindingTriageMenu'

vi.mock('@/api', () => ({
  analysisApi: {setFindingStatus: vi.fn().mockResolvedValue({}), setFindingStatusBatch: vi.fn()},
}))

const finding: Finding = {
  id: 'f1',
  ruleId: 'unhandled-error',
  severity: 'warning',
  title: 'Unhandled error',
  description: 'No handler',
  blockId: 'b1',
  subflowId: 'sf1',
}

const org: Organisation = {
  id: 'org-1',
  name: 'Ops',
  ownerId: 'u1',
  createdAt: '',
  updatedAt: '',
  members: [{userId: 'u2', role: 'member', joinedAt: '', user: {id: 'u2', email: 'b@x.io', displayName: 'Bea'}}],
}

function renderMenu(status: FindingStatus['status']) {
  const key = findingKey(finding)
  useAnalysisStore.setState({triageMap: new Map([[key, {flowId: 'flow-1', findingKey: key, status, updatedAt: ''}]])})
  render(
    <ToastProvider>
      <ConfirmProvider>
        <FindingTriageMenu finding={finding} />
      </ConfirmProvider>
    </ToastProvider>,
  )
  fireEvent.click(screen.getByTitle('Set triage status'))
}

beforeEach(() => {
  useAnalysisStore.getState().reset()
  useOrgStore.setState({organisations: [org]})
})

describe('FindingTriageMenu', () => {
  it('offers assignment on a status it knows', () => {
    renderMenu('acknowledged')
    expect(screen.getAllByText('Acknowledged').length).toBeGreaterThan(0)
    expect(screen.getByText('Assign to…')).toBeInTheDocument()
  })

  it('shows a status a newer server added under a neutral chip, and offers no reassignment', () => {
    renderMenu(unknownValue('needs_review'))
    expect(screen.getByText('Other status')).toBeInTheDocument()
    expect(screen.queryByText(/needs_review|triage\.status/)).toBeNull()
    expect(screen.queryByText('Assign to…')).toBeNull()
    // Every known status can still be chosen: that request sends a known value.
    expect(screen.getByRole('button', {name: 'Resolved'})).toBeInTheDocument()
  })
})
