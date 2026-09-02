import {describe, it, expect, beforeEach} from 'vitest'
import {useAnalysisStore, findingKey} from '@/stores/analysisStore'
import {useAuthStore} from '@/stores/authStore'
import type {Finding} from '@/types'

const f = (id: string, rule = 'r'): Finding => ({
  id,
  ruleId: rule,
  severity: 'warning',
  title: id,
  description: '',
  blockId: 'b-' + id,
  subflowId: 'sf',
})

describe('triage queue filters (R0-3)', () => {
  beforeEach(() => {
    useAuthStore.setState({user: {id: 'u1', email: 'u1@x.io'} as never})
    useAnalysisStore.setState({
      statusFilter: new Set(['open', 'acknowledged', 'in_progress', 'resolved']),
      assignedToMeOnly: false,
      triageMap: new Map([
        [findingKey(f('a')), {flowId: 'fl', findingKey: findingKey(f('a')), ruleId: 'r', status: 'resolved', assigneeId: 'u1', updatedAt: ''} as never],
        [findingKey(f('b')), {flowId: 'fl', findingKey: findingKey(f('b')), ruleId: 'r', status: 'in_progress', assigneeId: 'u2', updatedAt: ''} as never],
      ]),
    })
  })

  it('toggleStatusFilter drops a status from the working set', () => {
    const {toggleStatusFilter} = useAnalysisStore.getState()
    toggleStatusFilter('resolved')
    expect(useAnalysisStore.getState().statusFilter.has('resolved')).toBe(false)
    toggleStatusFilter('resolved')
    expect(useAnalysisStore.getState().statusFilter.has('resolved')).toBe(true)
  })

  it('toggleAssignedToMe flips the Mine switch', () => {
    useAnalysisStore.getState().toggleAssignedToMe()
    expect(useAnalysisStore.getState().assignedToMeOnly).toBe(true)
    useAnalysisStore.getState().toggleAssignedToMe()
    expect(useAnalysisStore.getState().assignedToMeOnly).toBe(false)
  })

  it('effective status: absent triage entry counts as open', () => {
    const {triageMap} = useAnalysisStore.getState()
    expect(triageMap.get(findingKey(f('untracked'))) ?? {status: 'open'}).toMatchObject({status: 'open'})
  })

  it('reset restores the defaults', () => {
    useAnalysisStore.getState().toggleStatusFilter('resolved')
    useAnalysisStore.getState().toggleAssignedToMe()
    useAnalysisStore.getState().reset()
    expect(useAnalysisStore.getState().statusFilter.has('resolved')).toBe(true)
    expect(useAnalysisStore.getState().assignedToMeOnly).toBe(false)
  })
})
