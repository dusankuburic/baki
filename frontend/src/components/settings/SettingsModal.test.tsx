import {describe, it, expect, vi, beforeEach, onTestFinished} from 'vitest'
import {render, screen, fireEvent, act, waitFor} from '@testing-library/react'
import SettingsModal from './SettingsModal'
import {ConfirmProvider} from '@/components/shared'
import {useUIStore} from '@/stores/uiStore'
import {simulateTauri, simulateWeb} from '@/testing/testHelpers'

vi.mock('@/api', async importOriginal => {
  const actual = await importOriginal<typeof import('@/api')>()
  const {ApiError} = await import('@/api/client')
  return {
    ...actual,
    providersApi: {
      ...actual.providersApi,
      getProviderCatalog: vi.fn().mockRejectedValue(new ApiError('not found', 404, null)),
      getGitHubUser: vi.fn().mockResolvedValue(null),
      getCopilotUser: vi.fn().mockResolvedValue(null),
      hasApiKey: vi.fn().mockResolvedValue(false),
      getLocalModelsConfig: vi.fn().mockResolvedValue({configured: false, baseUrl: ''}),
      listAzureConnections: vi.fn().mockResolvedValue({
        connections: [],
        registryState: 'ok',
        limits: {connections: 10, models: 50},
        knowledgeEmbeddingEnabled: false,
      }),
      listAzureModels: vi.fn().mockResolvedValue({models: []}),
    },
  }
})

async function flushFrame() {
  await act(async () => {
    await new Promise(resolve => requestAnimationFrame(() => resolve(null)))
  })
}

const noop = () => {}
const tab = (name: string) => screen.getByRole('tab', {name})

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

    fireEvent.keyDown(search, {key: 'Escape'})
    expect(onClose).not.toHaveBeenCalled()
    expect(search).toHaveValue('')
    expect(screen.getAllByRole('tab').length).toBeGreaterThan(1)

    fireEvent.keyDown(search, {key: 'Escape'})
    expect(onClose).toHaveBeenCalledTimes(1)
  })
  it('follows a deep link that arrives while it is open, even to a section asked for before', async () => {
    render(<SettingsModal isOpen onClose={noop} />)
    await screen.findByText('Updates')

    act(() => useUIStore.getState().openSettings('privacy'))
    expect(tab('Privacy')).toHaveAttribute('aria-selected', 'true')
    expect(await screen.findByText('API Keys')).toBeInTheDocument()
    expect(useUIStore.getState().settingsSection).toBeNull()

    fireEvent.click(tab('Shortcuts'))
    await screen.findByPlaceholderText('Filter shortcuts…')
    act(() => useUIStore.getState().openSettings('privacy'))
    expect(tab('Privacy')).toHaveAttribute('aria-selected', 'true')
    await screen.findByText('API Keys')
  })

  it('consumes the section it opened on, and ignores one this platform does not have', async () => {
    const restore = simulateTauri()
    try {
      useUIStore.setState({settingsSection: 'privacy'})
      render(<SettingsModal isOpen onClose={noop} initialSection="privacy" />)
      expect(tab('Privacy')).toHaveAttribute('aria-selected', 'true')
      expect(useUIStore.getState().settingsSection).toBeNull()
      await screen.findByText('API Keys')

      act(() => useUIStore.getState().openSettings('orgs'))
      expect(tab('Privacy')).toHaveAttribute('aria-selected', 'true')
      expect(useUIStore.getState().settingsSection).toBeNull()
    } finally {
      restore()
    }
  })

  it('follows a deep link into AI Accounts and focuses its anchor once the panel has loaded', async () => {
    render(
      <ConfirmProvider>
        <SettingsModal isOpen onClose={noop} />
      </ConfirmProvider>,
    )
    await screen.findByText('Updates')
    act(() => useUIStore.getState().openSettings('accounts', 'provider-azure-foundry'))
    expect(tab('AI Accounts')).toHaveAttribute('aria-selected', 'true')
    const heading = await screen.findByRole('heading', {level: 3, name: 'Azure AI Foundry'}, {timeout: 5000})
    await waitFor(() => expect(heading).toHaveFocus())
    expect(useUIStore.getState().settingsAnchor).toBeNull()
    await screen.findByText('Connect an Azure AI Foundry resource')
  })

  it('lands a deep link that opens it on its heading when the panel is ready at once, and gives focus back on close', async () => {
    const first = render(
      <ConfirmProvider>
        <SettingsModal isOpen onClose={noop} initialSection="accounts" />
      </ConfirmProvider>,
    )
    await screen.findByText('Connect an Azure AI Foundry resource', undefined, {timeout: 5000})
    await flushFrame()
    first.unmount()

    const trigger = document.createElement('button')
    trigger.textContent = 'Model for this chat'
    document.body.appendChild(trigger)
    onTestFinished(() => trigger.remove())
    trigger.focus()
    act(() => useUIStore.getState().openSettings('accounts', 'provider-azure-foundry'))
    const second = render(
      <ConfirmProvider>
        <SettingsModal isOpen onClose={noop} initialSection={useUIStore.getState().settingsSection} />
      </ConfirmProvider>,
    )
    const heading = await screen.findByRole('heading', {level: 3, name: 'Azure AI Foundry'})
    await flushFrame()
    await flushFrame()
    expect(heading).toHaveFocus()
    expect(useUIStore.getState().settingsAnchor).toBeNull()

    second.rerender(
      <ConfirmProvider>
        <SettingsModal isOpen={false} onClose={noop} />
      </ConfirmProvider>,
    )
    expect(trigger).toHaveFocus()
  })

  it('drops an unspent anchor when the user picks a section by hand', async () => {
    render(<SettingsModal isOpen onClose={noop} />)
    await screen.findByText('Updates')
    act(() => useUIStore.getState().openSettings('shortcuts', 'provider-azure-foundry'))
    await screen.findByPlaceholderText('Filter shortcuts…')
    expect(useUIStore.getState().settingsAnchor).toBe('provider-azure-foundry')
    fireEvent.click(tab('About'))
    expect(useUIStore.getState().settingsAnchor).toBeNull()
    await screen.findByText('Built with Tauri, Go, React, and TypeScript.')
  })

  it('starts a new section at the top, before a deep-linked panel scrolls to its anchor', async () => {
    render(<SettingsModal isOpen onClose={noop} />)
    await screen.findByText('Updates')
    const body = screen.getByRole('tabpanel')
    body.scrollTop = 500
    act(() => useUIStore.getState().openSettings('about'))
    expect(body.scrollTop).toBe(0)
    await screen.findByText('Built with Tauri, Go, React, and TypeScript.')
  })

  it('finds the AI Accounts and Organizations sections when searching for the Azure AI Foundry provider', async () => {
    render(<SettingsModal isOpen onClose={noop} />)
    fireEvent.change(screen.getByRole('textbox', {name: 'Search settings'}), {target: {value: 'foundry'}})
    expect(screen.getAllByRole('tab')).toHaveLength(2)
    expect(tab('AI Accounts')).toBeInTheDocument()
    expect(tab('Organizations')).toBeInTheDocument()
    await screen.findByText('Updates')
  })
})
