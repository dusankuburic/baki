import {describe, it, expect, vi} from 'vitest'
import {render, screen, fireEvent, within} from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import ChatHeader from './ChatHeader'
import {ConfirmProvider} from '@/components/shared/ConfirmDialog'
import type {PickerProvider} from './ModelPicker'
import type {ModelDetail} from '@/types'

// ChatHeader is the panel's only row of chrome — it absorbed the provider
// selector, the model selector (now the ModelPicker: each thread keeps its
// own model), the connection-status row and the whole action toolbar. These
// cover the wiring that used to live in ChatToolbar.test.tsx.

const models: ModelDetail[] = [
  {id: 'm1', displayName: 'Sonnet', contextLimit: 200_000, inputCostPerM: 3, outputCostPerM: 15},
  {id: 'm2', displayName: 'Haiku', contextLimit: 100_000, inputCostPerM: 1, outputCostPerM: 5},
]

const providers: PickerProvider[] = [{id: 'claude', name: 'Claude', models}]

function renderHeader(overrides: Partial<Parameters<typeof ChatHeader>[0]> = {}) {
  const props = {
    providers,
    provider: 'claude',
    model: 'm1',
    modelLabel: 'Sonnet',
    modelDetail: models[0],
    defaultModels: {claude: 'm1'},
    onPickModel: vi.fn(),
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

const picker = () => screen.getByRole('button', {name: /^Model for this chat:/})

async function openPicker(user: ReturnType<typeof userEvent.setup>) {
  await user.click(picker())
  return screen.getByRole('dialog', {name: 'Choose a model for this chat'})
}

describe('ChatHeader', () => {
  it("names the thread's model first, with the provider in the trigger's name", () => {
    renderHeader()
    expect(picker()).toHaveAccessibleName('Model for this chat: Sonnet, Claude')
    expect(within(picker()).getByText('Sonnet')).toBeVisible()
    // The connection state stays on the trigger's hover title.
    expect(picker()).toHaveAttribute('title', 'Ready')
  })

  it('badges a model an organisation shares — icon and text, the org named for screen readers', () => {
    const shared: ModelDetail = {
      id: 'm/review0000',
      displayName: 'Review bot',
      contextLimit: 128_000,
      inputCostPerM: 0,
      outputCostPerM: 0,
      kind: 'agent',
      purpose: 'review',
      shared: true,
    }
    renderHeader({
      providers: [{id: 'azure-foundry', name: 'Azure AI Foundry', models: [shared]}],
      provider: 'azure-foundry',
      model: shared.id,
      modelLabel: 'Review bot',
      modelDetail: shared,
      orgName: 'Contoso',
    })
    expect(picker()).toHaveAccessibleName(
      'Model for this chat: Review bot, Review & analysis, Azure AI Foundry, shared by Contoso',
    )
    expect(within(picker()).getByText('Shared')).toBeVisible()
    expect(within(picker()).getByText('Shared by Contoso')).toHaveClass('sr-only')
  })

  it("badges a provider whose credentials are the organisation's", () => {
    renderHeader({providers: [{id: 'claude', name: 'Claude', models, shared: true}]})
    expect(picker()).toHaveAccessibleName(
      "Model for this chat: Sonnet, Claude, using your organisation's shared credentials",
    )
    expect(within(picker()).getByText('Shared')).toBeVisible()
  })

  it('picks a model for the thread from the picker', async () => {
    const user = userEvent.setup()
    const props = renderHeader()
    await openPicker(user)
    await user.click(screen.getByRole('option', {name: /^Haiku/}))
    expect(props.onPickModel).toHaveBeenCalledWith('claude', models[1])
  })

  it('shows the model but offers no picks without a thread', () => {
    renderHeader({pickerDisabled: true})
    expect(picker()).toBeDisabled()
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

  it('keeps tools available on a provider without native tool calling, and says so', async () => {
    const user = userEvent.setup()
    const onToggleTools = vi.fn()
    renderHeader({
      useTools: false,
      onToggleTools,
      modelDetail: {...models[0], supportsTools: false},
    })
    await openOverflow(user)
    // The chat service runs these through the text-marker fallback, so the
    // item must stay enabled rather than be greyed out.
    const item = screen.getByRole('menuitem', {name: 'Tools off · no native tools'})
    expect(item).toBeEnabled()
    await user.click(item)
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
})

describe('ChatHeader Foundry agent tools item (F-39)', () => {
  const agent: ModelDetail = {
    id: 'm/triage0000',
    displayName: 'Triage',
    contextLimit: 128000,
    inputCostPerM: 0,
    outputCostPerM: 0,
    kind: 'agent',
  }
  const azure = (list: ModelDetail[]): PickerProvider[] => [
    {id: 'azure-foundry', name: 'Azure AI Foundry', models: list},
  ]

  it('disables the tools toggle and explains it for a Foundry agent (by its kind)', async () => {
    const user = userEvent.setup()
    const onToggleTools = vi.fn()
    renderHeader({
      providers: azure([agent]),
      provider: 'azure-foundry',
      model: agent.id,
      modelLabel: 'Triage',
      modelDetail: agent,
      useTools: true,
      onToggleTools,
    })
    await openOverflow(user)
    const item = screen.getByRole('menuitem', {name: 'Tools: defined by the agent'})
    expect(item).toHaveAttribute('aria-disabled', 'true')
    // C14: it says why — and stays in the arrow-key order, so keyboard and
    // screen-reader users reach it (a plain `disabled` item was skipped).
    expect(item).toHaveAccessibleDescription('An agent runs the tools set up in its Azure AI Foundry definition')
    await user.keyboard('{ArrowDown}{ArrowDown}') // New chat → Tools
    expect(item).toHaveFocus()
    expect(screen.queryByRole('menuitem', {name: 'Tools on'})).toBeNull()
    expect(screen.queryByRole('menuitem', {name: 'Tools off'})).toBeNull()
    // Unavailable items swallow the click and Enter — the toggle stays inert.
    await user.keyboard('{Enter}')
    await user.click(item)
    expect(onToggleTools).not.toHaveBeenCalled()
  })

  it('knows a pre-registry agent by its prefix before the listing says', async () => {
    const user = userEvent.setup()
    renderHeader({
      providers: azure([]),
      provider: 'azure-foundry',
      model: 'agent:triage',
      modelLabel: 'agent:triage',
      modelDetail: undefined,
      onToggleTools: vi.fn(),
    })
    await openOverflow(user)
    expect(screen.getByRole('menuitem', {name: 'Tools: defined by the agent'})).toHaveAttribute('aria-disabled', 'true')
  })

  it('keeps the live on/off toggle for a non-agent model', async () => {
    const user = userEvent.setup()
    const onToggleTools = vi.fn()
    const chat = {...agent, id: 'm/depa000000', kind: 'chat' as const}
    renderHeader({
      providers: azure([chat]),
      provider: 'azure-foundry',
      model: chat.id,
      modelLabel: 'dep-a',
      modelDetail: chat,
      useTools: true,
      onToggleTools,
    })
    await openOverflow(user)
    expect(screen.getByRole('menuitem', {name: 'Tools on'})).toBeInTheDocument()
    expect(screen.queryByRole('menuitem', {name: 'Tools: defined by the agent'})).toBeNull()
  })
})

describe('ChatHeader context window formatting (F-44)', () => {
  it('renders a 1M context window as "1M", not "1000k"', async () => {
    const user = userEvent.setup()
    const big: ModelDetail = {
      id: 'big',
      displayName: 'Fable',
      contextLimit: 1_000_000,
      inputCostPerM: 10,
      outputCostPerM: 50,
    }
    renderHeader({providers: [{id: 'claude', name: 'Claude', models: [big]}], model: 'big', modelLabel: 'Fable'})
    await openPicker(user)
    expect(screen.getByRole('option', {name: /^Fable/})).toHaveAccessibleName('Fable 1M context window')
    expect(screen.queryByText(/1000k/)).toBeNull()
  })

  it('keeps sub-1M windows as integer k', async () => {
    const user = userEvent.setup()
    renderHeader()
    await openPicker(user)
    expect(screen.getByRole('option', {name: /^Sonnet/})).toHaveAccessibleName(
      'Sonnet Default for new chats 200k context window',
    )
  })
})

describe('ChatHeader unpriced models', () => {
  // Azure AI Foundry is not priced by baki: its deployments list at 0/0 and
  // the picker must show neither "$0" nor an estimate.
  it('shows no price on a zero-priced model', async () => {
    const user = userEvent.setup()
    const gpt: ModelDetail = {
      id: 'gpt-4o',
      displayName: 'gpt-4o',
      contextLimit: 128_000,
      inputCostPerM: 0,
      outputCostPerM: 0,
    }
    renderHeader({
      providers: [{id: 'azure-foundry', name: 'Azure AI Foundry', models: [gpt]}],
      provider: 'azure-foundry',
      model: 'gpt-4o',
      modelLabel: 'gpt-4o',
      modelDetail: gpt,
      defaultModels: {},
    })
    await openPicker(user)
    const item = screen.getByRole('option', {name: /^gpt-4o/})
    expect(item.textContent).not.toContain('$')
  })
})

describe('ChatHeader focus mode and text size', () => {
  it('offers a focus toggle beside More actions', () => {
    const onToggleFocus = vi.fn()
    renderHeader({onToggleFocus})
    fireEvent.click(screen.getByRole('button', {name: 'Focus mode'}))
    expect(onToggleFocus).toHaveBeenCalledTimes(1)
    expect(screen.getByRole('button', {name: 'More actions'})).toBeInTheDocument()
  })

  it('labels the toggle as the way out while focused', () => {
    renderHeader({onToggleFocus: vi.fn(), isFocused: true})
    expect(screen.getByRole('button', {name: 'Exit focus mode'})).toHaveAttribute('aria-pressed', 'true')
  })

  it('lists focus mode and the text size in the overflow menu', async () => {
    const user = userEvent.setup()
    const onCycleTextSize = vi.fn()
    renderHeader({onToggleFocus: vi.fn(), chatTextSize: 'md', onCycleTextSize})
    await openOverflow(user)
    expect(screen.getByRole('menuitem', {name: /Focus mode/})).toBeInTheDocument()
    await user.click(screen.getByRole('menuitem', {name: 'Text size: Medium'}))
    expect(onCycleTextSize).toHaveBeenCalledTimes(1)
  })
})
