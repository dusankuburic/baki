import {describe, it, expect, vi, beforeEach} from 'vitest'

const searchFlow = vi.fn()
vi.mock('@/api', () => ({
  flowApi: {searchFlow: (...a: unknown[]) => searchFlow(...a)},
}))

import {runSearch} from './useSidebarSearch'
import {useSearchStore} from '@/stores/searchStore'
import {useFlowStore} from '@/stores/flowStore'

describe('runSearch', () => {
  beforeEach(() => {
    searchFlow.mockReset()
    useSearchStore.getState().clear()
    useFlowStore.setState({document: {id: 'flow-1'} as never})
  })

  it('sets the query and actually searches the open flow', async () => {
    searchFlow.mockResolvedValue({
      results: [
        {blockId: 'b1', subflowId: 'Main', matchedField: 'name', matchedText: 'Helper', score: 1, highlights: []},
      ],
      totalCount: 1,
    })

    await runSearch('Helper')

    expect(searchFlow).toHaveBeenCalledWith('flow-1', expect.objectContaining({text: 'Helper'}))
    const s = useSearchStore.getState()
    expect(s.query).toBe('Helper')
    expect(s.results.map(r => r.blockId)).toEqual(['b1'])
    expect(s.activeResultIndex).toBe(0)
  })

  it('drops a slower earlier search that lands after a newer one', async () => {
    let resolveFirst: (v: unknown) => void = () => {}
    searchFlow
      .mockImplementationOnce(() => new Promise(r => (resolveFirst = r)))
      .mockResolvedValueOnce({results: [], totalCount: 0})

    const first = runSearch('old')
    await runSearch('new')
    resolveFirst({
      results: [
        {blockId: 'stale', subflowId: 'Main', matchedField: 'name', matchedText: 'old', score: 1, highlights: []},
      ],
      totalCount: 1,
    })
    await first

    expect(useSearchStore.getState().query).toBe('new')
    expect(useSearchStore.getState().results).toEqual([])
  })
})
