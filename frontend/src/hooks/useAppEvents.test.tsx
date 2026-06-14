import {describe, it, expect, vi, beforeEach} from 'vitest'
import {render} from '@testing-library/react'
import {useAppEvents} from './useAppEvents'
import {useFlowStore} from '@/stores/flowStore'
import {useAnalysisStore} from '@/stores/analysisStore'

let capturedCallback: ((ev: {name: string; data?: unknown}) => void) | null = null

vi.mock('@/api/client', () => ({
  subscribeToEvents: vi.fn(async (cb: (ev: {name: string; data?: unknown}) => void) => {
    capturedCallback = cb
    return () => { capturedCallback = null }
  }),
}))

function TestComponent({openDocument}: {openDocument: (doc: unknown) => void}) {
  useAppEvents({openDocument})
  return null
}

beforeEach(() => {
  capturedCallback = null
  vi.clearAllMocks()
})

describe('useAppEvents', () => {
  it('forwards analysis:progress events to the analysis store', async () => {
    const openDocument = vi.fn()
    render(<TestComponent openDocument={openDocument} />)

    await new Promise(r => setTimeout(r, 10))

    expect(capturedCallback).not.toBeNull()
    capturedCallback!({
      name: 'analysis:progress',
      data: {current: 5, total: 10, ruleName: 'dead-code'},
    })

    const progress = useAnalysisStore.getState().progress
    expect(progress.current).toBe(5)
    expect(progress.total).toBe(10)
    expect(progress.ruleName).toBe('dead-code')
  })

  it('forwards flow:parse-progress to the flow store', async () => {
    const openDocument = vi.fn()
    render(<TestComponent openDocument={openDocument} />)

    await new Promise(r => setTimeout(r, 10))

    capturedCallback!({
      name: 'flow:parse-progress',
      data: {percent: 42},
    })

    expect(useFlowStore.getState().isParsing).toBe(true)
    expect(useFlowStore.getState().parseProgress).toBe(42)
  })

  it('calls openDocument when flow:loaded event arrives', async () => {
    const openDocument = vi.fn()
    render(<TestComponent openDocument={openDocument} />)

    await new Promise(r => setTimeout(r, 10))

    const docData = {id: 'test-1', name: 'Test'}
    capturedCallback!({name: 'flow:loaded', data: docData})

    expect(openDocument).toHaveBeenCalledWith(docData)
  })

  it('forwards flow:load-error to the flow store', async () => {
    const openDocument = vi.fn()
    render(<TestComponent openDocument={openDocument} />)

    await new Promise(r => setTimeout(r, 10))

    capturedCallback!({
      name: 'flow:load-error',
      data: {error: 'file not readable'},
    })

    expect(useFlowStore.getState().parseError).toBe('file not readable')
  })

  it('ignores unknown event names', async () => {
    const openDocument = vi.fn()
    render(<TestComponent openDocument={openDocument} />)

    await new Promise(r => setTimeout(r, 10))

    capturedCallback!({name: 'unknown:event', data: {foo: 'bar'}})

    expect(openDocument).not.toHaveBeenCalled()
  })
})
