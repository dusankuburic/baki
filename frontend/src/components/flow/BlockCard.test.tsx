import {describe, it, expect, vi, beforeEach} from 'vitest'
import {render, screen, fireEvent, waitFor} from '@testing-library/react'
import BlockCard from './BlockCard'
import {useFlowStore} from '@/stores/flowStore'
import {ToastProvider} from '@/components/shared/Toast'
import {ConfirmProvider} from '@/components/shared/ConfirmDialog'
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
      <ConfirmProvider>
        <BlockCard block={baseBlock} {...overrides} />
      </ConfirmProvider>
    </ToastProvider>,
  )
}

beforeEach(() => {
  vi.clearAllMocks()
  useFlowStore.setState({document: {id: 'flow-1', name: 'F'} as never, expandedBlockIds: new Set()})
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
        <ConfirmProvider>
          <BlockCard block={baseBlock} hasFindings />
        </ConfirmProvider>
      </ToastProvider>,
    )
    fireEvent.contextMenu(screen.getByRole('button'))
    expect(screen.getByText('Fix with AI')).toBeInTheDocument()
  })

  const containerBlock: Block = {
    ...baseBlock,
    id: 'loop1',
    type: 'LOOP',
    rawType: 'LOOP',
    name: 'Loop 1..10',
    children: [baseBlock],
  }

  it('renders the expand chevron for container blocks', () => {
    renderCard({block: containerBlock})
    const chevron = screen.getByRole('button', {name: /collapse block/i})
    expect(chevron).toHaveAttribute('aria-expanded', 'true')
  })

  it('leaf blocks have no chevron (no expand control in the leading slot)', () => {
    renderCard()
    expect(screen.queryByRole('button', {name: /expand block/i})).not.toBeInTheDocument()
    expect(screen.queryByRole('button', {name: /collapse block/i})).not.toBeInTheDocument()
  })

  it('toggling the chevron stops propagation and flips the store expansion', () => {
    renderCard({block: containerBlock})
    fireEvent.click(screen.getByRole('button', {name: /collapse block/i}))
    expect(useFlowStore.getState().expandedBlockIds.has('loop1')).toBe(true)
    expect(screen.getByRole('button', {name: /expand block/i})).toHaveAttribute('aria-expanded', 'false')

    fireEvent.click(screen.getByRole('button', {name: /expand block/i}))
    expect(useFlowStore.getState().expandedBlockIds.has('loop1')).toBe(false)
    expect(screen.getByRole('button', {name: /collapse block/i})).toHaveAttribute('aria-expanded', 'true')
  })

  it('shows the collapsed children count only for collapsed containers', () => {
    const {rerender} = renderCard({block: containerBlock})
    expect(screen.queryByText(/items/i)).not.toBeInTheDocument()

    useFlowStore.getState().toggleBlockExpand('loop1')
    rerender(
      <ToastProvider>
        <ConfirmProvider>
          <BlockCard block={containerBlock} />
        </ConfirmProvider>
      </ToastProvider>,
    )
    expect(screen.getByText(/1 items/i)).toBeInTheDocument()
  })

  it('renders the findings count inside the header row (in flow, not absolute)', () => {
    renderCard({findingCount: 3, findingSeverity: 'error'})
    const card = screen.getByRole('button', {name: /ACTION: Display Message/i})
    const badge = screen.getByText('3')
    expect(badge.className).not.toContain('absolute')
    expect(card.contains(badge)).toBe(true)
    const group = badge.parentElement
    expect(group).not.toBeNull()
    expect(group!.className).toContain('ml-auto')
  })
})

// ── Block editing (R3-1b) ────────────────────────────────────────────────────

const removeBlock = vi.fn()
const duplicateBlock = vi.fn()
const listSnapshots = vi.fn()
const restoreSnapshot = vi.fn()

vi.mock('@/api', () => ({
  analysisApi: {analyzeFlow: vi.fn().mockResolvedValue(null), analyzeFlowById: vi.fn().mockResolvedValue(null)},
  flowApi: {
    removeBlock: (...a: unknown[]) => removeBlock(...a),
    duplicateBlock: (...a: unknown[]) => duplicateBlock(...a),
    listSnapshots: (...a: unknown[]) => listSnapshots(...a),
    restoreSnapshot: (...a: unknown[]) => restoreSnapshot(...a),
  },
}))

describe('BlockCard block editing', () => {
  beforeEach(() => {
    removeBlock.mockResolvedValue({document: {id: 'flow-1', name: 'F', subflows: [{id: 'sf1', name: 'Main', blocks: []}]}})
    duplicateBlock.mockResolvedValue({document: {id: 'flow-1', name: 'F', subflows: [{id: 'sf1', name: 'Main', blocks: []}]}})
  })

  // The context menu itself is a shared component; the contract under test is
  // the wiring: the menu items exist and invoke the right endpoint. The menu
  // opens on right-click of the card header.
  const openMenu = (container: HTMLElement) => {
    const card = container.querySelector('[data-testid="block-card"], [role="button"]') ?? container.firstElementChild!
    fireEvent.contextMenu(card)
  }

  it('move up/down reorders and offers a per-action Undo (U1.4)', async () => {
    const mockDoc = {id: 'flow-1', name: 'F', subflows: [{id: 'sf1', name: 'Main', blocks: []}]}
    const moveBlock = vi.fn().mockResolvedValue({document: mockDoc})
    const {flowApi} = await import('@/api')
    const real = flowApi.moveBlock
    flowApi.moveBlock = moveBlock as never
    listSnapshots.mockResolvedValue({snapshots: [{id: 'snap-move', label: 'before remove', createdAt: new Date().toISOString(), bytes: 10}]})
    const restoreSnapshot2 = vi.fn().mockResolvedValue({document: mockDoc})
    const realRestore = flowApi.restoreSnapshot
    flowApi.restoreSnapshot = restoreSnapshot2 as never

    const {container} = renderCard()
    const openMenu = (c: HTMLElement) => {
      const card = c.querySelector('[data-testid="block-card"], [role="button"]') ?? c.firstElementChild!
      fireEvent.contextMenu(card)
    }
    openMenu(container)
    fireEvent.click(await screen.findByText('Move up'))
    await waitFor(() => expect(moveBlock).toHaveBeenCalledWith('flow-1', 'b1', 'up'))

    // Undo toast appears and restores THIS action's captured snapshot id.
    const undoBtn = await screen.findByText('Undo')
    fireEvent.click(undoBtn)
    await waitFor(() => expect(restoreSnapshot2).toHaveBeenCalledWith('flow-1', 'snap-move'))

    flowApi.moveBlock = real
    flowApi.restoreSnapshot = realRestore
  })

  it('offers Duplicate and Delete in the context menu and calls the endpoints', async () => {
    const {container} = renderCard()
    openMenu(container)
    const dup = await screen.findByText('Duplicate block')
    fireEvent.click(dup)
    await waitFor(() => expect(duplicateBlock).toHaveBeenCalledWith('flow-1', 'b1'), {timeout: 1500})

    // Item clicks close the menu — reopen for the second action. Delete now
    // confirms first (U1.3); accept the dialog.
    openMenu(container)
    const del = await screen.findByText('Delete block')
    fireEvent.click(del)
    const confirmBtn = await screen.findByRole('button', {name: 'Delete'})
    fireEvent.click(confirmBtn)
    await waitFor(() => expect(removeBlock).toHaveBeenCalledWith('flow-1', 'b1'))
  })
})

describe('BlockCard read-only gating (U1.2)', () => {
  it('hides mutation items from the context menu on read-only flows', () => {
    useFlowStore.setState({readOnly: true})
    const {container} = renderCard()
    fireEvent.contextMenu(container.firstElementChild as HTMLElement)
    expect(screen.getByText('Explain with AI')).toBeInTheDocument()
    expect(screen.getByText('Copy as Markdown')).toBeInTheDocument()
    for (const label of ['Edit properties…', 'Move up', 'Move down', 'Duplicate block', 'Delete block']) {
      expect(screen.queryByText(label)).not.toBeInTheDocument()
    }
  })

  it('shows mutation items when the flow is editable', () => {
    useFlowStore.setState({readOnly: false})
    const {container} = renderCard()
    fireEvent.contextMenu(container.firstElementChild as HTMLElement)
    expect(screen.getByText('Edit properties…')).toBeInTheDocument()
    expect(screen.getByText('Delete block')).toBeInTheDocument()
  })
})

describe('BlockCard delete confirmation (U1.3)', () => {
  it('confirms before deleting and aborts on cancel', async () => {
    const removeBlock = vi.fn().mockResolvedValue({document: null})
    const {flowApi} = await import('@/api')
    const real = flowApi.removeBlock
    flowApi.removeBlock = removeBlock as never

    const {container} = render(
      <ToastProvider>
        <ConfirmProvider>
          <BlockCard block={baseBlock} />
        </ConfirmProvider>
      </ToastProvider>,
    )
    fireEvent.contextMenu(container.firstElementChild as HTMLElement)
    fireEvent.click(screen.getByText('Delete block'))

    // The danger dialog is up with the block name; cancel must not call the API.
    expect(await screen.findByText('Delete block', {selector: 'h2, [role="heading"], .text-base'})).toBeTruthy()
    fireEvent.click(await screen.findByRole('button', {name: /cancel/i}))
    expect(removeBlock).not.toHaveBeenCalled()

    flowApi.removeBlock = real
  })
})

describe('inline rename (U3b)', () => {
  it('double-click on a COMMENT name opens the rename input; Enter commits via renameBlock', async () => {
    const renameBlock = vi.fn().mockResolvedValue({document: {id: 'flow-1', name: 'F', subflows: []}, gotoRefsUpdated: 0})
    const {flowApi} = await import('@/api')
    const real = flowApi.renameBlock
    flowApi.renameBlock = renameBlock as never

    const commentBlock = {...baseBlock, rawType: 'COMMENT', name: 'old text'}
    const {container} = renderCard({block: commentBlock as Block})
    // Double-click the name area (the title div below the type row).
    const nameEl = Array.from(container.querySelectorAll('div')).find(d => d.textContent === 'old text')!
    fireEvent.doubleClick(nameEl)
    const input = await screen.findByTestId('rename-input')
    expect((input as HTMLInputElement).value).toBe('old text')

    fireEvent.change(input, {target: {value: 'new text'}})
    fireEvent.keyDown(input, {key: 'Enter'})
    await waitFor(() => expect(renameBlock).toHaveBeenCalledWith('flow-1', commentBlock.id, 'new text'))
    flowApi.renameBlock = real
  })

  it('Escape cancels without calling the API', async () => {
    const renameBlock = vi.fn()
    const {flowApi} = await import('@/api')
    const real = flowApi.renameBlock
    flowApi.renameBlock = renameBlock as never

    const commentBlock = {...baseBlock, rawType: 'COMMENT', name: 'keep me'}
    renderCard({block: commentBlock as Block})
    useFlowStore.setState({renamingBlockId: commentBlock.id} as never)
    const input = await screen.findByTestId('rename-input')
    fireEvent.keyDown(input, {key: 'Escape'})
    expect(renameBlock).not.toHaveBeenCalled()
    expect(useFlowStore.getState().renamingBlockId).toBeNull()
    flowApi.renameBlock = real
  })

  it('action blocks never open the rename affordance', () => {
    const {container} = renderCard()
    const nameEl = Array.from(container.querySelectorAll('div')).find(d => d.textContent === 'Display Message')!
    fireEvent.doubleClick(nameEl)
    expect(screen.queryByTestId('rename-input')).toBeNull()
  })
})

describe('interaction polish (V1)', () => {
  it('context menu shows keyboard shortcut chips for the edit actions', () => {
    const {container} = renderCard()
    fireEvent.contextMenu(container.firstElementChild as HTMLElement)
    const row = (label: string) => screen.getByText(label).closest('button') as HTMLElement
    expect(row('Move up').textContent).toContain('Alt')
    expect(row('Move down').textContent).toContain('Alt')
    expect(row('Duplicate block').textContent).toContain('Ctrl')
    expect(row('Delete block').textContent).toContain('Del')
  })

  it('Rename (with F2 hint) appears only for LABEL/COMMENT blocks', () => {
    const {container} = renderCard()
    fireEvent.contextMenu(container.firstElementChild as HTMLElement)
    expect(screen.queryByText('Rename')).toBeNull()

    const commentCard = renderCard({block: {...baseBlock, rawType: 'COMMENT'} as Block})
    fireEvent.contextMenu(commentCard.container.firstElementChild as HTMLElement)
    const rename = screen.getByText('Rename').closest('button') as HTMLElement
    expect(rename.textContent).toContain('F2')
  })

  it('props hint exists for property-bearing blocks', () => {
    const withProps = {...baseBlock, properties: {Url: 'https://x', Timeout: '30', _derived: 'skip'}} as Block
    const {container} = renderCard({block: withProps})
    const hint = container.querySelector('[data-testid="props-hint"]')
    expect(hint).toBeInTheDocument()
    expect(hint?.textContent).toContain('3 props')
  })

  it('group-selected cards carry the selection ring when not singly selected', () => {
    useFlowStore.setState({selectedBlockIds: new Set([baseBlock.id])} as never)
    const {container, unmount} = renderCard()
    const card = container.firstElementChild as HTMLElement
    expect(card.className).toContain('ring-brand-500/30')
    unmount()
    useFlowStore.setState({selectedBlockIds: new Set()} as never)
  })
})
