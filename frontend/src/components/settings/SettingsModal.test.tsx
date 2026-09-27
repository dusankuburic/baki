import {describe, it, expect, vi, beforeEach} from 'vitest'
import {render, screen, fireEvent, act} from '@testing-library/react'
import SettingsModal from './SettingsModal'
import {useUIStore} from '@/stores/uiStore'
import {simulateTauri, simulateWeb} from '@/testing/testHelpers'

// Modal focuses inside a rAF; let that frame run before asserting on focus.
async function flushFrame() {
  await act(async () => {
    await new Promise(resolve => requestAnimationFrame(() => resolve(null)))
  })
}

const noop = () => {}
const tab = (name: string) => screen.getByRole('tab', {name})

// Only sections whose panels need no API or providers are visited here
// (General, Appearance, Shortcuts, Privacy, About); each test waits for the
// lazy panel so the load never lands outside act().
beforeEach(() => {
  simulateWeb()
  useUIStore.setState({settingsOpen: true, settingsSection: null, lastSettingsSection: null})
})

describe('SettingsModal', () => {
  it('opens on General with a vertical tablist and keeps the dialog name the e2e suite pins', async () => {
    render(<SettingsModal isOpen onClose={noop} />)
    expect(screen.getByRole('dialog', {name: 'Settings'})).toBeInTheDocument()
    expect(screen.getByRole('tablist', {name: 'Settings sections'})).toHaveAttribute('aria-orientation', 'vertical')
    expect(tab('General')).toHaveAttribute('aria-selected', 'true')
    expect(tab('General')).toHaveAttribute('tabindex', '0')
    expect(tab('Appearance')).toHaveAttribute('tabindex', '-1')
    expect(screen.getByRole('tabpanel', {name: 'General'})).toBeInTheDocument()
    expect(screen.getByRole('heading', {level: 2, name: 'General'})).toBeInTheDocument()
    expect(await screen.findByText('Updates')).toBeInTheDocument()
  })

  it('focuses the selected tab on open', async () => {
    render(<SettingsModal isOpen onClose={noop} />)
    await flushFrame()
    expect(document.activeElement).toBe(tab('General'))
    await screen.findByText('Updates')
  })

  it('opens on the deep-linked section and remembers the last one shown', async () => {
    const first = render(<SettingsModal isOpen onClose={noop} initialSection="privacy" />)
    expect(tab('Privacy')).toHaveAttribute('aria-selected', 'true')
    expect(await screen.findByText('API Keys')).toBeInTheDocument()

    fireEvent.click(tab('Shortcuts'))
    expect(screen.getByRole('heading', {level: 2, name: 'Shortcuts'})).toBeInTheDocument()
    await screen.findByPlaceholderText('Filter shortcuts…')
    first.unmount()
    expect(useUIStore.getState().lastSettingsSection).toBe('shortcuts')

    render(<SettingsModal isOpen onClose={noop} />)
    expect(tab('Shortcuts')).toHaveAttribute('aria-selected', 'true')
    await screen.findByPlaceholderText('Filter shortcuts…')
  })

  it('falls back to General when the remembered section is not available on this platform', async () => {
    useUIStore.setState({lastSettingsSection: 'orgs'})
    const restore = simulateTauri()
    try {
      render(<SettingsModal isOpen onClose={noop} />)
      expect(tab('General')).toHaveAttribute('aria-selected', 'true')
      expect(screen.queryByRole('tab', {name: 'Organizations'})).toBeNull()
      expect(screen.queryByRole('tab', {name: 'Policy Gates'})).toBeNull()
      await screen.findByText('Updates')
    } finally {
      restore()
    }
  })

  it('moves the selection with the arrow keys, wrapping at both ends', async () => {
    render(<SettingsModal isOpen onClose={noop} />)
    fireEvent.keyDown(tab('General'), {key: 'ArrowDown'})
    expect(tab('Appearance')).toHaveAttribute('aria-selected', 'true')
    expect(document.activeElement).toBe(tab('Appearance'))
    fireEvent.keyDown(tab('Appearance'), {key: 'End'})
    expect(tab('About')).toHaveAttribute('aria-selected', 'true')
    fireEvent.keyDown(tab('About'), {key: 'ArrowDown'})
    expect(tab('General')).toHaveAttribute('aria-selected', 'true')
    fireEvent.keyDown(tab('General'), {key: 'ArrowUp'})
    expect(tab('About')).toHaveAttribute('aria-selected', 'true')
    expect(await screen.findByText('Built with Tauri, Go, React, and TypeScript.')).toBeInTheDocument()
  })

  it('filters the sections, opens the first match on Enter and clears the query before it closes on Escape', async () => {
    const onClose = vi.fn()
    render(<SettingsModal isOpen onClose={onClose} />)
    const search = screen.getByRole('textbox', {name: 'Search settings'})

    fireEvent.change(search, {target: {value: 'theme'}})
    expect(screen.getAllByRole('tab')).toHaveLength(1)
    expect(tab('Appearance')).toHaveAttribute('tabindex', '0')

    fireEvent.keyDown(search, {key: 'Enter'})
    expect(tab('Appearance')).toHaveAttribute('aria-selected', 'true')
    expect(await screen.findByText('Color Theme')).toBeInTheDocument()

    fireEvent.change(search, {target: {value: 'zzz'}})
    expect(screen.queryAllByRole('tab')).toHaveLength(0)
    expect(screen.getByText('No settings match "zzz"')).toBeInTheDocument()

    // One Escape clears the query (and must not reach the dialog)…
    fireEvent.keyDown(search, {key: 'Escape'})
    expect(onClose).not.toHaveBeenCalled()
    expect(search).toHaveValue('')
    expect(screen.getAllByRole('tab').length).toBeGreaterThan(1)

    // …the next one closes.
    fireEvent.keyDown(search, {key: 'Escape'})
    expect(onClose).toHaveBeenCalledTimes(1)
  })
  it('finds the AI Accounts section when searching for the Azure AI Foundry provider', async () => {
    render(<SettingsModal isOpen onClose={noop} />)
    fireEvent.change(screen.getByRole('textbox', {name: 'Search settings'}), {target: {value: 'foundry'}})
    expect(screen.getAllByRole('tab')).toHaveLength(1)
    expect(tab('AI Accounts')).toBeInTheDocument()
    await screen.findByText('Updates')
  })
})
