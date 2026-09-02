import {describe, it, expect, vi, beforeEach} from 'vitest'
import {render, screen, fireEvent, waitFor} from '@testing-library/react'
import FindingsToolbar from './FindingsToolbar'
import {ToastProvider} from '@/components/shared/Toast'
import {ConfirmProvider} from '@/components/shared/ConfirmDialog'

const noop = vi.fn()

function baseProps() {
  return {
    onReanalyze: noop,
    onShowDiff: noop,
    diffLoading: false,
    onToggleDedup: noop,
    dedupActive: false,
    dedupLoading: false,
    onExportCSV: noop,
    onExportHTML: noop,
    onExportSARIF: noop,
    onExportJUnit: noop,
    onShare: noop,
    snapshots: [],
    snapshotsLoading: false,
    onRestoreSnapshot: noop,
    onOpenSnapshots: noop,
    sortMode: 'severity' as const,
    onSetSortMode: noop,
    hasFindings: true,
  }
}

beforeEach(() => vi.clearAllMocks())

// U1.5: the toolbar popovers (saved views, undo ring) close on outside click
// and Escape — they previously stuck open until their trigger was re-clicked.
describe('FindingsToolbar popover dismissal (U1.5)', () => {
  it('closes the undo popover on outside mousedown', async () => {
    render(
      <ToastProvider>
        <ConfirmProvider>
          <FindingsToolbar {...baseProps()} />
        </ConfirmProvider>
      </ToastProvider>,
    )
    fireEvent.click(screen.getByRole('button', {name: /undo a fix/i}))
    expect(await screen.findByText(/No snapshots yet/)).toBeInTheDocument()
    fireEvent.mouseDown(document.body)
    await waitFor(() => expect(screen.queryByText(/No snapshots yet/)).not.toBeInTheDocument())
  })

  it('closes the undo popover on Escape', async () => {
    render(
      <ToastProvider>
        <ConfirmProvider>
          <FindingsToolbar {...baseProps()} />
        </ConfirmProvider>
      </ToastProvider>,
    )
    fireEvent.click(screen.getByRole('button', {name: /undo a fix/i}))
    expect(await screen.findByText(/No snapshots yet/)).toBeInTheDocument()
    fireEvent.keyDown(document, {key: 'Escape'})
    await waitFor(() => expect(screen.queryByText(/No snapshots yet/)).not.toBeInTheDocument())
  })

  it('closes the saved-views popover on outside mousedown', async () => {
    render(
      <ToastProvider>
        <ConfirmProvider>
          <FindingsToolbar {...baseProps()} />
        </ConfirmProvider>
      </ToastProvider>,
    )
    fireEvent.click(screen.getByRole('button', {name: /saved filter views/i}))
    expect(await screen.findByText('Save current filters…')).toBeInTheDocument()
    fireEvent.mouseDown(document.body)
    await waitFor(() => expect(screen.queryByText('Save current filters…')).not.toBeInTheDocument())
  })
})

describe('sort dropdown + chip counts (U2.3)', () => {
  it('replaces the cycle button: opens a 3-option menu and applies the pick', async () => {
    const onSetSortMode = vi.fn()
    render(
      <ToastProvider>
        <ConfirmProvider>
          <FindingsToolbar {...baseProps()} onSetSortMode={onSetSortMode} />
        </ConfirmProvider>
      </ToastProvider>,
    )
    fireEvent.click(screen.getByRole('button', {name: /change findings sort order/i}))
    expect(await screen.findByRole('menuitemradio', {name: /by count/i})).toBeInTheDocument()
    expect(screen.getByRole('menuitemradio', {name: /by severity/i})).toHaveAttribute('aria-checked', 'true')
    fireEvent.click(screen.getByRole('menuitemradio', {name: /by count/i}))
    expect(onSetSortMode).toHaveBeenCalledWith('count')
    // Choosing closes the menu.
    await waitFor(() => expect(screen.queryByRole('menu')).not.toBeInTheDocument())
  })

  it('sort menu closes on Escape', async () => {
    render(
      <ToastProvider>
        <ConfirmProvider>
          <FindingsToolbar {...baseProps()} />
        </ConfirmProvider>
      </ToastProvider>,
    )
    fireEvent.click(screen.getByRole('button', {name: /change findings sort order/i}))
    expect(await screen.findByRole('menu')).toBeInTheDocument()
    fireEvent.keyDown(document, {key: 'Escape'})
    await waitFor(() => expect(screen.queryByRole('menu')).not.toBeInTheDocument())
  })

  it('severity chips show live counts and use semantic tone classes', () => {
    render(
      <ToastProvider>
        <ConfirmProvider>
          <FindingsToolbar {...baseProps()} severityCounts={{error: 3, warning: 0, info: 12}} />
        </ConfirmProvider>
      </ToastProvider>,
    )
    const errors = screen.getByText('Errors').closest('button')
    expect(errors).toHaveTextContent('3')
    expect(errors?.className).toContain('text-semantic-error')
    // Zero counts don't render a ghost 0.
    expect(screen.getByText('Warnings').closest('button')).toHaveTextContent('Warnings')
  })
})
