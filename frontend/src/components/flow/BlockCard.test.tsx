import {describe, it, expect, vi, beforeEach} from 'vitest'
import {render, screen, fireEvent} from '@testing-library/react'
import BlockCard from './BlockCard'
import {useFlowStore} from '@/stores/flowStore'
import {ToastProvider} from '@/components/shared/Toast'
import type {Block} from '@/types'

vi.mock('@/lib/clipboard', () => ({
  writeClipboard: vi.fn().mockResolvedValue(undefined),
}))

vi.mock('@/lib/fixWithAI', () => ({
  stageBlockPrompt: vi.fn(),
}))

vi.mock('@/lib/blocks', async () => {
  const actual = await vi.importActual<typeof import('@/lib/blocks')>('@/lib/blocks')
  return actual
})

const baseBlock: Block = {
  subflowId: 'sf1',
  id: 'b1',
  name: 'Display Message',
  type: 'ACTION',
  rawType: 'ACTION',
  indent: 0,
  lineNumber: 1,
  children: [],
  properties: {},
  variables: [],
}

function renderCard(overrides: Partial<Parameters<typeof BlockCard>[0]> = {}) {
  return render(
    <ToastProvider>
      <BlockCard block={baseBlock} {...overrides} />
    </ToastProvider>,
  )
}

beforeEach(() => {
  vi.clearAllMocks()
  useFlowStore.setState({document: {id: 'flow-1', name: 'F'} as never})
})

describe('BlockCard', () => {
  it('renders the block name and is keyboard focusable', () => {
    renderCard()
    const el = screen.getByRole('button', {name: /ACTION: Display Message/i})
    expect(el).toHaveAttribute('tabIndex', '0')
    expect(el).toHaveAttribute('aria-pressed', 'false')
  })

  it('announces selection state via aria-pressed', () => {
    renderCard({selected: true})
    expect(screen.getByRole('button', {name: /Display Message/i})).toHaveAttribute('aria-pressed', 'true')
  })

  it('selects via click and Enter/Space keys', () => {
    const onClick = vi.fn()
    renderCard({onClick})

    const el = screen.getByRole('button')
    fireEvent.click(el)
    expect(onClick).toHaveBeenCalledTimes(1)

    fireEvent.keyDown(el, {key: ' '})
    expect(onClick).toHaveBeenCalledTimes(2)

    fireEvent.keyDown(el, {key: 'Enter'})
    expect(onClick).toHaveBeenCalledTimes(3)
  })

  it('opens via Enter on an already-selected card (double-click equivalent)', () => {
    const onClick = vi.fn()
    const onDoubleClick = vi.fn()
    renderCard({selected: true, onClick, onDoubleClick})

    fireEvent.keyDown(screen.getByRole('button'), {key: 'Enter'})
    expect(onDoubleClick).toHaveBeenCalledTimes(1)
    // Selected+Enter routes to open, not re-select.
    expect(onClick).not.toHaveBeenCalled()
  })

  it('opens the context menu on right click and offers copy actions', async () => {
    const {writeClipboard} = await import('@/lib/clipboard')
    renderCard()

    fireEvent.contextMenu(screen.getByRole('button'))

    const copyId = await screen.findByText('Copy Block ID')
    fireEvent.click(copyId)

    await vi.waitFor(() => {
      expect(writeClipboard).toHaveBeenCalledWith('b1')
    })
  })

  it('adds the Fix with AI action only when findings exist', () => {
    const {rerender} = renderCard({hasFindings: false})
    fireEvent.contextMenu(screen.getByRole('button'))
    expect(screen.queryByText('Fix with AI')).not.toBeInTheDocument()

    rerender(
      <ToastProvider>
        <BlockCard block={baseBlock} hasFindings />
      </ToastProvider>,
    )
    fireEvent.contextMenu(screen.getByRole('button'))
    expect(screen.getByText('Fix with AI')).toBeInTheDocument()
  })
})
