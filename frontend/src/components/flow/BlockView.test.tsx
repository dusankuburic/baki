import {describe, it, expect, vi, beforeEach} from 'vitest'
import {render, screen, fireEvent, waitFor} from '@testing-library/react'
import BlockView, {blockCanvasScrollers} from './BlockView'
import {useFlowStore} from '@/stores/flowStore'
import {ToastProvider} from '@/components/shared/Toast'
import {ConfirmProvider} from '@/components/shared/ConfirmDialog'
import type {Block, FlowDocument} from '@/types'

// The DnD affordance is desktop-gated (HTML5 DnD never fires on touch);
// jsdom's matchMedia stub reports a phone-ish viewport, so force desktop.
vi.mock('@/hooks/useMediaQuery', () => ({
  useIsDesktop: () => true,
}))

// Virtuoso is virtualized and measures nothing in jsdom — render flat.
vi.mock('react-virtuoso', () => ({
  Virtuoso: ({
    data = [],
    itemContent,
    computeItemKey,
  }: {
    data?: unknown[]
    itemContent: (i: number, item: unknown) => React.ReactNode
    computeItemKey?: (i: number, item: unknown) => string | number
  }) => (
    <div>
      {data.map((item, i) => (
        <div key={computeItemKey ? computeItemKey(i, item) : i}>{itemContent(i, item)}</div>
      ))}
    </div>
  ),
}))

const moveBlockTo = vi.fn()
const analyzeFlow = vi.fn()

vi.mock('@/api', () => ({
  flowApi: {
    moveBlockTo: (...a: unknown[]) => moveBlockTo(...a),
  },
  analysisApi: {
    analyzeFlow: (...a: unknown[]) => analyzeFlow(...a),
    analyzeFlowById: (...a: unknown[]) => analyzeFlow(...a),
  },
}))

const b = (id: string, name: string, type: Block['type'] = 'ACTION'): Block => ({
  subflowId: 'sf1',
  id,
  name,
  type,
  rawType: 'HTTPClient.InvokeUrl',
  indent: 0,
  lineNumber: 1,
  children: [],
  properties: {},
  variables: [],
})

const doc = {
  id: 'flow-1',
  name: 'F',
  subflows: [
    {
      id: 'sf1',
      name: 'Main',
      blocks: [
        b('b1', 'First'),
        b('b2', 'Second'),
        {
          ...b('cond1', 'Check'),
          type: 'CONDITION' as Block['type'],
          children: [b('inner1', 'Nested')],
        },
        b('b3', 'Third'),
      ],
    },
  ],
} as unknown as FlowDocument

function renderView() {
  useFlowStore.setState({selectedBlockIds: new Set(), renamingBlockId: null} as never)
  return render(
    <ToastProvider>
      <ConfirmProvider>
        <BlockView />
      </ConfirmProvider>
    </ToastProvider>,
  )
}

beforeEach(() => {
  vi.clearAllMocks()
  analyzeFlow.mockResolvedValue(null)
  // A distinct marker doc so the store swap is observable.
  const updatedDoc = {...doc, name: 'F-updated'} as unknown as FlowDocument
  moveBlockTo.mockResolvedValue({document: updatedDoc})
  useFlowStore.setState({document: doc, selectedBlockId: null, readOnly: false} as never)
})

// Layout effect polyfill guard: jsdom lacks getBoundingClientRect positioning —
// rows report height 0, making every clientY land "after" (>= top+0). The
// tests therefore assert the after-position contract and the wiring, not the
// half-row split (a real-browser concern).
describe('BlockView drag-to-reorder (R3.3)', () => {
  it('marks editable rows draggable and structural rows not', () => {
    const endDoc = {
      ...doc,
      subflows: [{id: 'sf1', name: 'Main', blocks: [...doc.subflows[0].blocks, b('e1', 'End', 'END')]}],
    } as unknown as FlowDocument
    useFlowStore.setState({document: endDoc} as never)
    renderView()
    const first = document.querySelector('[data-block-id="b1"]') as HTMLElement
    const end = document.querySelector('[data-block-id="e1"]') as HTMLElement
    expect(first.draggable).toBe(true)
    expect(end.draggable).toBe(false)
  })

  it('a drop maps to moveBlockTo with the dragged id, the row id, and a position', async () => {
    renderView()
    const target = document.querySelector('[data-block-id="b2"]') as HTMLElement
    fireEvent.drop(target, {
      dataTransfer: {getData: () => 'b1', types: {includes: () => true}},
      clientY: 0,
    })
    await waitFor(() => expect(moveBlockTo).toHaveBeenCalled())
    const [flowId, draggedId, refId, position] = moveBlockTo.mock.calls[0]
    expect(flowId).toBe('flow-1')
    expect(draggedId).toBe('b1')
    expect(refId).toBe('b2')
    expect(position === 'before' || position === 'after').toBe(true)
    // Success: the doc swaps in and re-analysis fires.
    await waitFor(() => expect(useFlowStore.getState().document?.name).toBe('F-updated'))
    expect(analyzeFlow).toHaveBeenCalledWith('flow-1')
  })

  it('read-only flows render rows inert (no draggable, no drop handling)', () => {
    useFlowStore.setState({readOnly: true} as never)
    renderView()
    const row = document.querySelector('[data-block-id="b1"]') as HTMLElement
    expect(row.draggable).toBe(false)
    fireEvent.drop(row, {
      dataTransfer: {getData: () => 'b2', types: {includes: () => true}},
      clientY: 0,
    })
    expect(moveBlockTo).not.toHaveBeenCalled()
  })

  it('a self-drop is ignored', async () => {
    renderView()
    const target = document.querySelector('[data-block-id="b1"]') as HTMLElement
    fireEvent.drop(target, {
      dataTransfer: {getData: () => 'b1', types: {includes: () => true}},
      clientY: 0,
    })
    expect(moveBlockTo).not.toHaveBeenCalled()
  })

  it('a refused move surfaces the guard error without corrupting state', async () => {
    moveBlockTo.mockRejectedValue(new Error('the target block is not a sibling'))
    renderView()
    const target = document.querySelector('[data-block-id="b3"]') as HTMLElement
    fireEvent.drop(target, {
      dataTransfer: {getData: () => 'b1', types: {includes: () => true}},
      clientY: 0,
    })
    await waitFor(() => expect(moveBlockTo).toHaveBeenCalled())
    // No document swap on failure.
    expect(useFlowStore.getState().document?.name).toBe('F')
    await waitFor(() => expect(screen.getByText('Move failed')).toBeInTheDocument())
  })
})

describe('multi-select bulk actions (U3b)', () => {
  it('shift-click selects the visible range from the anchor', () => {
    renderView()
    const st = useFlowStore.getState()
    st.selectBlock('b1')
    const card = document.querySelector('[data-block-id="b3"] [role="button"]') as HTMLElement
    fireEvent.click(card, {shiftKey: true})
    // Visible order: b1, b2, cond1, inner1, b3 — the RANGE spans all five.
    expect(useFlowStore.getState().selectedBlockIds).toEqual(new Set(['b1', 'b2', 'cond1', 'inner1', 'b3']))
  })

  it('cmd-click toggles individual blocks into the selection', () => {
    renderView()
    const card = (id: string) => document.querySelector(`[data-block-id="${id}"] [role="button"]`) as HTMLElement
    fireEvent.click(card('b2'), {metaKey: true})
    fireEvent.click(card('b3'), {metaKey: true})
    expect(useFlowStore.getState().selectedBlockIds).toEqual(new Set(['b2', 'b3']))
    fireEvent.click(card('b2'), {metaKey: true})
    expect(useFlowStore.getState().selectedBlockIds).toEqual(new Set(['b3']))
  })

  it('shows the bulk bar for a multi-selection and group-moves via the boundary sibling', async () => {
    renderView()
    const st = useFlowStore.getState()
    st.setBlockSelection(new Set(['b1', 'b2']))
    const bar = await screen.findByTestId('bulk-bar')
    expect(bar).toHaveTextContent('2 blocks selected')

    // Group DOWN of [b1,b2]: the top-level sibling AFTER the selection
    // (cond1) moves BEFORE the first selected block — one moveBlockTo call.
    fireEvent.click(screen.getByText('Move down'))
    await waitFor(() => expect(moveBlockTo).toHaveBeenCalledWith('flow-1', 'cond1', 'b1', 'before'))
  })

  it('refuses a group move when the selection is non-contiguous', async () => {
    renderView()
    // b1 and b3 selected, b2 in between → refuse with guidance, no API call.
    useFlowStore.getState().setBlockSelection(new Set(['b1', 'b3']))
    fireEvent.click(await screen.findByText('Move up'))
    expect(moveBlockTo).not.toHaveBeenCalled()
    expect(await screen.findByText('Group move needs a contiguous same-scope selection')).toBeInTheDocument()
  })

  it('bulk delete confirms then calls removeBlocks', async () => {
    const removeBlocks = vi.fn().mockResolvedValue({document: {...doc, name: 'F2'}})
    const {flowApi} = await import('@/api')
    flowApi.removeBlocks = removeBlocks as never
    renderView()
    useFlowStore.getState().setBlockSelection(new Set(['b1', 'b2']))
    fireEvent.click(await screen.findByText('Delete'))
    // Danger dialog → confirm with the DIALOG's Delete (the last rendered).
    const delBtns = await screen.findAllByRole('button', {name: 'Delete'})
    fireEvent.click(delBtns[delBtns.length - 1])
    await waitFor(() => expect(removeBlocks).toHaveBeenCalledWith('flow-1', ['b1', 'b2']))
  })

  it('no bulk bar on read-only flows', () => {
    useFlowStore.setState({readOnly: true} as never)
    renderView()
    useFlowStore.getState().setBlockSelection(new Set(['b1', 'b2']))
    expect(screen.queryByTestId('bulk-bar')).toBeNull()
  })
})

describe('BlockView canvas legibility (U3a)', () => {
  it('renders one indentation rail per ancestor depth', () => {
    renderView()
    const top = document.querySelector('[data-block-id="b1"]') as HTMLElement
    const nested = document.querySelector('[data-block-id="inner1"]') as HTMLElement
    expect(top.querySelectorAll('[data-testid="indent-rail"]')).toHaveLength(0)
    expect(nested.querySelectorAll('[data-testid="indent-rail"]')).toHaveLength(1)
    expect(nested.querySelector('[data-testid="indent-rail"]')?.getAttribute('data-depth')).toBe('1')
  })

  it('ignores foreign text/plain drags (custom MIME only)', () => {
    renderView()
    const target = document.querySelector('[data-block-id="b2"]') as HTMLElement
    fireEvent.dragOver(target, {
      dataTransfer: {types: {includes: (t: string) => t === 'text/plain'}, dropEffect: 'none'},
      clientY: 0,
    })
    expect(target.querySelector('[data-testid="drop-indicator"]')).toBeNull()
    fireEvent.drop(target, {
      dataTransfer: {
        getData: (t: string) => (t === 'application/x-baki-block' ? '' : 'foreign text'),
        types: {includes: (t: string) => t === 'text/plain'},
      },
      clientY: 0,
    })
    expect(moveBlockTo).not.toHaveBeenCalled()
  })

  it('accepts the block MIME type and opens a gap (animated margin)', () => {
    renderView()
    const target = document.querySelector('[data-block-id="b2"]') as HTMLElement
    fireEvent.dragOver(target, {
      dataTransfer: {types: {includes: (t: string) => t === 'application/x-baki-block'}, dropEffect: 'move'},
      clientY: 0, // jsdom rects are 0-height → the "after" half
    })
    const content = target.querySelector('[data-testid="row-content"]') as HTMLElement
    expect(content.style.marginBottom).toBe('10px')
    expect(content.style.marginTop).toBe('0px')
    expect(target.querySelector('[data-testid="drop-indicator"]')).not.toBeNull()
    fireEvent.dragLeave(target, {dataTransfer: {types: {includes: () => true}}})
    expect(content.style.marginBottom).toBe('0px')
  })

  it('auto-scrolls the canvas when dragging near the viewport edge', () => {
    const fake = {
      _scrollTop: 100,
      getBoundingClientRect: () =>
        ({top: 0, bottom: 600, height: 600, left: 0, right: 400, width: 400, x: 0, y: 0}) as DOMRect,
    }
    Object.defineProperty(fake, 'scrollTop', {
      get() {
        return fake._scrollTop
      },
      set(v: number) {
        fake._scrollTop = v
      },
    })
    blockCanvasScrollers.set('sf1', fake as unknown as HTMLElement)
    try {
      renderView()
      const target = document.querySelector('[data-block-id="b2"]') as HTMLElement
      const dragOverAt = (el: HTMLElement, clientY: number) => {
        const ev = new Event('dragover', {bubbles: true, cancelable: true})
        Object.defineProperty(ev, 'clientY', {value: clientY})
        Object.defineProperty(ev, 'dataTransfer', {
          value: {types: {includes: (t: string) => t === 'application/x-baki-block'}, dropEffect: 'move'},
        })
        fireEvent(el, ev)
      }
      // Within 64px of the BOTTOM edge → scrollTop += 14.
      dragOverAt(target, 590)
      expect(fake._scrollTop).toBe(114)
      // Top edge → scrolls back up.
      dragOverAt(target, 10)
      expect(fake._scrollTop).toBe(100)
    } finally {
      blockCanvasScrollers.clear()
    }
  })
})
