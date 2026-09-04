import {describe, it, expect, vi} from 'vitest'
import {render, screen} from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import ChatHeader from './ChatHeader'
import {ConfirmProvider} from '@/components/shared/ConfirmDialog'
import type {ModelDetail, ProviderID} from '@/types'

// ChatHeader is the panel's only row of chrome — it absorbed the provider
// selector, the model selector, the connection-status row and the whole action
// toolbar. These cover the wiring that used to live in ChatToolbar.test.tsx.

const providers = [
  {id: 'claude' as ProviderID, name: 'Claude', configured: true},
  {id: 'openai' as ProviderID, name: 'OpenAI', configured: false},
]

const models: ModelDetail[] = [
  {id: 'm1', displayName: 'Sonnet', contextLimit: 200_000, inputCostPerM: 3, outputCostPerM: 15},
  {id: 'm2', displayName: 'Haiku', contextLimit: 100_000, inputCostPerM: 1, outputCostPerM: 5},
]

function renderHeader(overrides: Partial<Parameters<typeof ChatHeader>[0]> = {}) {
  const props = {
    providers,
    selectedProvider: 'claude' as ProviderID,
    onSelectProvider: vi.fn(),
    models,
    selectedModel: 'm1',
    onSelectModel: vi.fn(),
    demoRemaining: null,
    messageCount: 3,
    useTools: false,
    onNewChat: vi.fn(),
    onClearContext: vi.fn(),
    onCompact: vi.fn(),
    ...overrides,
  }
  render(
    <ConfirmProvider>
      <ChatHeader {...props} />
    </ConfirmProvider>,
  )
  return props
}

async function openOverflow(user: ReturnType<typeof userEvent.setup>) {
  await user.click(screen.getByRole('button', {name: 'More actions'}))
}

describe('ChatHeader', () => {
  it('shows the provider and model on one line', () => {
    renderHeader()
    expect(screen.getByText('Claude')).toBeInTheDocument()
    expect(screen.getByText('· Sonnet')).toBeInTheDocument()
  })

  it('lists providers and models in a single menu and selects one', async () => {
    const user = userEvent.setup()
    const props = renderHeader()
    await user.click(screen.getByText('Claude'))

    // Unconfigured providers are offered as a route into settings, not hidden.
    expect(screen.getByRole('menuitem', {name: /OpenAI — configure/})).toBeInTheDocument()

    await user.click(screen.getByRole('menuitem', {name: /Haiku/}))
    expect(props.onSelectModel).toHaveBeenCalledWith('m2')
  })

  it('runs non-destructive overflow actions directly', async () => {
    const user = userEvent.setup()
    const props = renderHeader()
    await openOverflow(user)
    await user.click(screen.getByRole('menuitem', {name: 'New chat'}))
    expect(props.onNewChat).toHaveBeenCalled()
  })

  it('reflects the tools toggle state and fires it', async () => {
    const user = userEvent.setup()
    const onToggleTools = vi.fn()
    renderHeader({useTools: true, onToggleTools})
    await openOverflow(user)
    // Label states the CURRENT state, so the user can see tools are on.
    await user.click(screen.getByRole('menuitem', {name: 'Tools on'}))
    expect(onToggleTools).toHaveBeenCalled()
  })

  it('confirms before clearing instead of firing immediately', async () => {
    const user = userEvent.setup()
    const props = renderHeader()
    await openOverflow(user)
    await user.click(screen.getByRole('menuitem', {name: 'Clear'}))

    // The old inline strip auto-dismissed after 5s; this is a real dialog.
    expect(props.onClearContext).not.toHaveBeenCalled()
    await user.click(await screen.findByRole('button', {name: 'Clear'}))
    expect(props.onClearContext).toHaveBeenCalled()
  })

  it('disables message-scoped actions on an empty thread', async () => {
    const user = userEvent.setup()
    renderHeader({messageCount: 0, onExport: vi.fn(), onToggleSearch: vi.fn()})
    await openOverflow(user)
    for (const name of ['Clear', 'Compact', 'Export conversation', 'Search messages']) {
      expect(screen.getByRole('menuitem', {name})).toHaveAttribute('aria-disabled', 'true')
    }
  })

  it('shows the demo quota as a badge only for the demo provider', () => {
    const {unmount} = render(
      <ConfirmProvider>
        <ChatHeader
          providers={providers}
          selectedProvider="claude"
          onSelectProvider={vi.fn()}
          models={models}
          selectedModel="m1"
          onSelectModel={vi.fn()}
          demoRemaining={null}
          messageCount={0}
          useTools={false}
          onNewChat={vi.fn()}
          onClearContext={vi.fn()}
          onCompact={vi.fn()}
        />
      </ConfirmProvider>,
    )
    expect(screen.queryByText('4')).not.toBeInTheDocument()
    unmount()

    renderHeader({demoRemaining: 4})
    expect(screen.getByTitle('4 demo messages left today')).toBeInTheDocument()
  })
})
