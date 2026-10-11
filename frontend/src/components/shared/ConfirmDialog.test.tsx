import {describe, it, expect, vi} from 'vitest'
import {render, screen, fireEvent, within, waitFor} from '@testing-library/react'
import {ConfirmProvider, useConfirm} from './ConfirmDialog'
import Modal from './Modal'

type ConfirmOptions = Parameters<ReturnType<typeof useConfirm>['confirm']>[0]
type PromptOptions = Parameters<ReturnType<typeof useConfirm>['prompt']>[0]

function ConfirmTrigger({options, onResult}: {options: ConfirmOptions; onResult?: (ok: boolean) => void}) {
  const {confirm} = useConfirm()
  return <button onClick={() => void confirm(options).then(ok => onResult?.(ok))}>Open</button>
}

function PromptTrigger({options}: {options: PromptOptions}) {
  const {prompt} = useConfirm()
  return <button onClick={() => void prompt(options)}>Open</button>
}

describe('ConfirmDialog', () => {
  it('is named by its heading and described by its message', async () => {
    render(
      <ConfirmProvider>
        <ConfirmTrigger
          options={{title: 'Remove model?', message: 'It disappears from the chat picker.', danger: true}}
        />
      </ConfirmProvider>,
    )
    fireEvent.click(screen.getByRole('button', {name: 'Open'}))
    const dialog = await screen.findByRole('dialog', {name: 'Remove model?'})
    expect(dialog).toHaveAccessibleDescription('It disappears from the chat picker.')
  })

  it('is named by its message when it has no title', async () => {
    render(
      <ConfirmProvider>
        <ConfirmTrigger options={{message: 'Discard your changes?'}} />
      </ConfirmProvider>,
    )
    fireEvent.click(screen.getByRole('button', {name: 'Open'}))
    const dialog = await screen.findByRole('dialog', {name: 'Discard your changes?'})
    expect(dialog).not.toHaveAttribute('aria-describedby')
  })

  it("paints the danger action with the theme's error token, and neither button submits", async () => {
    render(
      <ConfirmProvider>
        <ConfirmTrigger options={{title: 'Remove?', message: 'Gone.', danger: true, confirmLabel: 'Remove'}} />
      </ConfirmProvider>,
    )
    fireEvent.click(screen.getByRole('button', {name: 'Open'}))
    const dialog = await screen.findByRole('dialog', {name: 'Remove?'})
    const remove = within(dialog).getByRole('button', {name: 'Remove'})
    expect(remove).toHaveClass('bg-semantic-error')
    expect(remove.className).toContain('text-(--error-foreground)')
    expect(remove.className).not.toMatch(/\bbg-red-/)
    expect(remove).toHaveAttribute('type', 'button')
    expect(within(dialog).getByRole('button', {name: 'Cancel'})).toHaveAttribute('type', 'button')
  })

  it('labels the prompt field with its label', async () => {
    render(
      <ConfirmProvider>
        <PromptTrigger options={{title: 'Rename', label: 'New name', initialValue: 'Flow 1'}} />
      </ConfirmProvider>,
    )
    fireEvent.click(screen.getByRole('button', {name: 'Open'}))
    const dialog = await screen.findByRole('dialog', {name: 'Rename'})
    expect(within(dialog).getByRole('textbox', {name: 'New name'})).toHaveValue('Flow 1')
  })
})

describe('ConfirmDialog over Settings', () => {
  function Settings({onClose, onResult}: {onClose: () => void; onResult: (ok: boolean) => void}) {
    const {confirm} = useConfirm()
    return (
      <Modal isOpen onClose={onClose} ariaLabel="Settings">
        <button
          onClick={() => void confirm({title: 'Remove?', message: 'Gone for good.', danger: true}).then(onResult)}
        >
          Remove resource
        </button>
      </Modal>
    )
  }

  it('closes only the confirm on Escape, even after Settings re-renders with a new onClose', async () => {
    const settingsClose = vi.fn()
    const onResult = vi.fn()
    const {rerender} = render(
      <ConfirmProvider>
        <Settings onClose={settingsClose} onResult={onResult} />
      </ConfirmProvider>,
    )
    fireEvent.click(screen.getByRole('button', {name: 'Remove resource'}))
    const confirmDialog = await screen.findByRole('dialog', {name: 'Remove?'})

    rerender(
      <ConfirmProvider>
        <Settings onClose={() => settingsClose()} onResult={onResult} />
      </ConfirmProvider>,
    )

    fireEvent.keyDown(within(confirmDialog).getByRole('button', {name: 'Cancel'}), {key: 'Escape'})
    await waitFor(() => expect(screen.queryByRole('dialog', {name: 'Remove?'})).not.toBeInTheDocument())
    expect(onResult).toHaveBeenCalledWith(false)
    expect(settingsClose).not.toHaveBeenCalled()
    expect(screen.getByRole('dialog', {name: 'Settings'})).toBeInTheDocument()

    fireEvent.keyDown(document.body, {key: 'Escape'})
    expect(settingsClose).toHaveBeenCalledTimes(1)
  })
})
