import {describe, it, expect, vi, beforeEach, afterEach} from 'vitest'
import {render, screen, within, act, waitFor} from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import ModelPicker, {type PickerProvider} from './ModelPicker'
import {useUIStore} from '@/stores/uiStore'
import {useChatStore} from '@/stores/chatStore'
import type {ModelDetail} from '@/types'

// ---- listings ----

const entry = (id: string, displayName: string, over: Partial<ModelDetail> = {}): ModelDetail => ({
  id: `m/${id.padEnd(10, '0')}`,
  displayName,
  contextLimit: 128_000,
  inputCostPerM: 0,
  outputCostPerM: 0,
  kind: 'chat',
  purpose: 'general',
  ...over,
})

const GPT = entry('gpt41', 'GPT-4.1', {contextLimit: 1_000_000})
const MINI = entry('mini', 'GPT-4.1 mini')
const DOCS = entry('docs', 'Docs writer', {
  kind: 'claude',
  purpose: 'documentation',
  contextLimit: 200_000,
  description: 'Writes flow documentation from the canvas',
})
const REVIEW = entry('review', 'Review bot', {kind: 'agent', purpose: 'review', shared: true})
const TESTER = entry('tester', 'Tester', {purpose: 'testing', status: 'needs_credentials'})
const SECURITY = entry('sec', 'Sentinel', {purpose: 'custom', purposeCustom: 'Security'})
const EMBED = entry('embed', 'Search index', {kind: 'embedding', purpose: undefined})

const AZURE: PickerProvider = {
  id: 'azure-foundry',
  name: 'Azure AI Foundry',
  models: [TESTER, REVIEW, DOCS, SECURITY, MINI, GPT, EMBED],
}
const SONNET: ModelDetail = {
  id: 'claude-sonnet-4-6',
  displayName: 'Claude Sonnet 4.6',
  contextLimit: 200_000,
  inputCostPerM: 3,
  outputCostPerM: 15,
}
const HAIKU: ModelDetail = {
  id: 'claude-haiku-4-5',
  displayName: 'Claude Haiku 4.5',
  contextLimit: 200_000,
  inputCostPerM: 1,
  outputCostPerM: 5,
  supportsTools: false,
}
const CLAUDE: PickerProvider = {id: 'claude', name: 'Claude', models: [SONNET, HAIKU]}

type PickerProps = Parameters<typeof ModelPicker>[0]

function renderPicker(over: Partial<PickerProps> = {}) {
  const props: PickerProps = {
    providers: [AZURE],
    provider: 'azure-foundry',
    model: DOCS.id,
    label: 'Docs writer',
    modelDetail: DOCS,
    defaults: {'azure-foundry': GPT.id},
    onSelect: vi.fn(),
    orgName: 'Contoso',
    ...over,
  }
  const view = render(
    <>
      <ModelPicker {...props} />
      <button type="button">after the picker</button>
    </>,
  )
  return {...view, props}
}

const trigger = () => screen.getByRole('button', {name: /^Model for this chat:/})
const dialog = () => screen.getByRole('dialog', {name: 'Choose a model for this chat'})
const listbox = () => within(dialog()).getByRole('listbox')
// Scoped to the listbox: the Provider select's own <option>s share the role.
const option = (name: RegExp) => within(listbox()).getByRole('option', {name})
const optionTexts = () =>
  within(listbox())
    .getAllByRole('option')
    .map(o => o.textContent)

async function openPicker(user: ReturnType<typeof userEvent.setup>) {
  await user.click(trigger())
  return dialog()
}

const originalOpenSettings = useUIStore.getState().openSettings
beforeEach(() => {
  useChatStore.setState({modelPickerRequest: 0})
})
afterEach(() => {
  useUIStore.setState({openSettings: originalOpenSettings})
})

describe('ModelPicker trigger', () => {
  it('names the model first, even at the default 320 px chat width', () => {
    render(
      <div className="chat-root" style={{width: 320}}>
        <ModelPicker
          providers={[AZURE]}
          provider="azure-foundry"
          model={REVIEW.id}
          label="Review bot"
          modelDetail={REVIEW}
          defaults={{}}
          onSelect={vi.fn()}
          orgName="Contoso"
        />
      </div>,
    )
    const button = screen.getByRole('button', {
      name: 'Model for this chat: Review bot, Review & analysis, Azure AI Foundry, shared by Contoso',
    })
    const name = within(button).getByText('Review bot')
    expect(name).toBeVisible()
    // The provider rides as an icon with its name in the tooltip.
    expect(within(button).getByTitle('Azure AI Foundry')).toBeInTheDocument()
    // The Shared badge is always shown, with its meaning spelled out.
    expect(within(button).getByText('Shared')).toBeVisible()
    expect(within(button).getByText('Shared by Contoso')).toHaveClass('sr-only')
  })

  it('names a catalogue model without a purpose, and says when nothing can be used', () => {
    const {rerender} = render(
      <ModelPicker
        providers={[CLAUDE]}
        provider="claude"
        model={SONNET.id}
        label="Claude Sonnet 4.6"
        modelDetail={SONNET}
        defaults={{}}
        onSelect={vi.fn()}
      />,
    )
    expect(screen.getByRole('button', {name: 'Model for this chat: Claude Sonnet 4.6, Claude'})).toBeInTheDocument()
    rerender(
      <ModelPicker providers={[AZURE]} provider="azure-foundry" model="" label="" defaults={{}} onSelect={vi.fn()} />,
    )
    expect(
      screen.getByRole('button', {name: 'Model for this chat: none available, Azure AI Foundry'}),
    ).toHaveTextContent('No model')
  })

  it('names a provider that is not configured here from the catalogue', () => {
    render(<ModelPicker providers={[CLAUDE]} provider="openai" model="" label="" defaults={{}} onSelect={vi.fn()} />)
    expect(screen.getByRole('button', {name: 'Model for this chat: none available, OpenAI'})).toBeInTheDocument()
  })

  it('opens on ArrowDown and on a request from the chat ("Choose model")', async () => {
    const user = userEvent.setup()
    renderPicker()
    trigger().focus()
    await user.keyboard('{ArrowDown}')
    expect(dialog()).toBeInTheDocument()
    expect(trigger()).toHaveAttribute('aria-expanded', 'true')
    await user.keyboard('{Escape}')
    expect(screen.queryByRole('dialog')).toBeNull()

    act(() => useChatStore.getState().openModelPicker())
    expect(dialog()).toBeInTheDocument()
  })

  it('does not open without a thread to pin a pick to', () => {
    renderPicker({disabled: true})
    expect(trigger()).toBeDisabled()
    act(() => useChatStore.getState().openModelPicker())
    expect(screen.queryByRole('dialog')).toBeNull()
  })

  // C9: the logout reset takes the request counter back to 0.
  it('opens only on a request, never on the counter going back (the logout reset)', () => {
    useChatStore.setState({modelPickerRequest: 3})
    renderPicker()
    act(() => useChatStore.setState({modelPickerRequest: 0}))
    expect(screen.queryByRole('dialog')).toBeNull()
    act(() => useChatStore.getState().openModelPicker())
    expect(dialog()).toBeInTheDocument()
  })

  // C8: the chat moves focus to the trigger after a notice is dismissed.
  it('hands its trigger to a ref the chat passes', () => {
    const triggerRef: {current: HTMLButtonElement | null} = {current: null}
    renderPicker({triggerRef})
    expect(triggerRef.current).toBe(trigger())
  })
})

describe('ModelPicker list', () => {
  it('groups by purpose — presets in their fixed order, then custom ones — and never lists embeddings', async () => {
    const user = userEvent.setup()
    renderPicker()
    const popup = await openPicker(user)
    const groups = within(popup).getAllByRole('group')
    expect(
      groups.map(
        g =>
          g.getAttribute('aria-labelledby') && document.getElementById(g.getAttribute('aria-labelledby')!)?.textContent,
      ),
    ).toEqual(['General', 'Documentation', 'Review & analysis', 'Testing', 'Security'])
    expect(
      within(groups[0])
        .getAllByRole('option')
        .map(o => o.textContent),
    ).toEqual([expect.stringMatching(/^GPT-4\.1/), expect.stringMatching(/^GPT-4\.1 mini/)])
    expect(within(popup).queryByRole('option', {name: /Search index/})).toBeNull()
  })

  it('shows the badges, the window and the description of each entry', async () => {
    const user = userEvent.setup()
    renderPicker()
    await openPicker(user)
    const gpt = option(/^GPT-4\.1 Default/)
    expect(gpt).toHaveAccessibleName('GPT-4.1 Default for new chats 1M context window')
    expect(within(gpt).getByText('Default')).toBeVisible()
    expect(option(/^Docs writer/)).toHaveAccessibleName('Docs writer Claude 200k context window')
    expect(option(/^Docs writer/)).toHaveAccessibleDescription('Writes flow documentation from the canvas')
    // The thread's model is the selected option.
    expect(option(/^Docs writer/)).toHaveAttribute('aria-selected', 'true')
    expect(option(/^Review bot/)).toHaveAccessibleName('Review bot Agent Shared by Contoso 128k context window')
  })

  it('keeps an entry whose resource needs credentials listed, disabled, with the reason', async () => {
    const user = userEvent.setup()
    const {props} = renderPicker()
    await openPicker(user)
    const tester = option(/^Tester/)
    expect(tester).toHaveAttribute('aria-disabled', 'true')
    expect(tester).toHaveAccessibleDescription("Its resource's credentials need to be entered again")
    await user.click(tester)
    expect(props.onSelect).not.toHaveBeenCalled()
    expect(dialog()).toBeInTheDocument()
  })

  it('moves between options with the arrows — across groups, never onto a header — and Enter picks', async () => {
    const user = userEvent.setup()
    const {props} = renderPicker()
    await openPicker(user)
    const list = within(dialog()).getByRole('listbox', {name: 'Models'})
    // Seven or fewer models: no search box, focus lands on the list, on the
    // thread's own model.
    expect(list).toHaveFocus()
    expect(within(dialog()).queryByRole('combobox')).toBeNull()
    expect(list).toHaveAttribute('aria-activedescendant', option(/^Docs writer/).id)

    await user.keyboard('{ArrowDown}')
    expect(list).toHaveAttribute('aria-activedescendant', option(/^Review bot/).id)
    await user.keyboard('{ArrowUp}{ArrowUp}')
    expect(list).toHaveAttribute('aria-activedescendant', option(/^GPT-4\.1 mini/).id)
    await user.keyboard('{Enter}')

    expect(props.onSelect).toHaveBeenCalledWith('azure-foundry', MINI)
    expect(screen.queryByRole('dialog')).toBeNull()
    expect(trigger()).toHaveFocus()
  })

  it('a disabled entry is reachable (its reason is read) but Enter does nothing', async () => {
    const user = userEvent.setup()
    const {props} = renderPicker()
    await openPicker(user)
    const list = within(dialog()).getByRole('listbox')
    await user.keyboard('{End}')
    expect(list).toHaveAttribute('aria-activedescendant', option(/^Sentinel/).id)
    await user.keyboard('{ArrowUp}')
    expect(list).toHaveAttribute('aria-activedescendant', option(/^Tester/).id)
    await user.keyboard('{Enter}')
    expect(props.onSelect).not.toHaveBeenCalled()
  })

  it('offers a search box above seven models; typing filters, and the box drives the list', async () => {
    const user = userEvent.setup()
    const many: PickerProvider = {
      ...AZURE,
      models: [
        ...AZURE.models,
        entry('extra1', 'Refactor helper', {purpose: 'refactoring'}),
        entry('extra2', 'Unit tests', {purpose: 'testing'}),
      ],
    }
    const {props} = renderPicker({providers: [many]})
    await openPicker(user)
    const search = within(dialog()).getByRole('combobox', {name: 'Search models'})
    expect(search).toHaveFocus()
    expect(search).toHaveAttribute('aria-controls', within(dialog()).getByRole('listbox').id)

    await user.type(search, 'documentation')
    // The purpose label is searched too.
    expect(optionTexts()).toEqual([expect.stringMatching(/^Docs writer/)])
    await user.keyboard('{ArrowDown}')
    expect(search).toHaveAttribute('aria-activedescendant', option(/^Docs writer/).id)
    await user.keyboard('{Enter}')
    expect(props.onSelect).toHaveBeenCalledWith('azure-foundry', DOCS)
  })

  it('says so when nothing matches', async () => {
    const user = userEvent.setup()
    const many: PickerProvider = {
      ...AZURE,
      models: [...AZURE.models, entry('extra1', 'One'), entry('extra2', 'Two')],
    }
    renderPicker({providers: [many]})
    await openPicker(user)
    await user.type(within(dialog()).getByRole('combobox'), 'zzz')
    expect(within(dialog()).getByRole('status')).toHaveTextContent('No models match “zzz”.')
  })

  it('a catalogue provider keeps its price and "no native tools" line', async () => {
    const user = userEvent.setup()
    renderPicker({
      providers: [CLAUDE],
      provider: 'claude',
      model: SONNET.id,
      label: SONNET.displayName,
      modelDetail: SONNET,
    })
    await openPicker(user)
    const group = within(dialog()).getByRole('group')
    expect(group).toHaveAccessibleName('Models')
    expect(option(/^Claude Sonnet 4\.6/)).toHaveAccessibleDescription('$3 / $15 per 1M tokens')
    expect(option(/^Claude Haiku 4\.5/)).toHaveAccessibleDescription('$1 / $5 per 1M tokens · no native tools')
  })
})

describe('ModelPicker keyboard and focus', () => {
  it('Escape clears the search first, then closes and returns focus — never reaching the chat', async () => {
    const user = userEvent.setup()
    const many: PickerProvider = {...AZURE, models: [...AZURE.models, entry('x1', 'One'), entry('x2', 'Two')]}
    renderPicker({providers: [many]})
    // The chat's own Escape (stop the reply, leave focus mode) listens on the
    // document, and skips a claimed key.
    const reachedDocument: string[] = []
    const chatEscape = (e: KeyboardEvent) => reachedDocument.push(e.key)
    document.addEventListener('keydown', chatEscape)
    try {
      await openPicker(user)
      const search = within(dialog()).getByRole('combobox')
      await user.type(search, 'docs')

      await user.keyboard('{Escape}')
      expect(search).toHaveValue('')
      expect(dialog()).toBeInTheDocument()

      await user.keyboard('{Escape}')
      expect(screen.queryByRole('dialog')).toBeNull()
      expect(trigger()).toHaveFocus()
      expect(reachedDocument).not.toContain('Escape')
    } finally {
      document.removeEventListener('keydown', chatEscape)
    }
  })

  it('Escape is claimed (defaultPrevented) even where it reaches no handler of the chat', async () => {
    const user = userEvent.setup()
    renderPicker()
    await openPicker(user)
    const list = within(dialog()).getByRole('listbox')
    let prevented: boolean | null = null
    list.addEventListener('keydown', e => {
      queueMicrotask(() => {
        prevented = e.defaultPrevented
      })
    })
    await user.keyboard('{Escape}')
    await waitFor(() => expect(prevented).toBe(true))
  })

  it('tabs through Provider → search → list → Manage models…, and closes when focus leaves', async () => {
    const user = userEvent.setup()
    const many: PickerProvider = {...AZURE, models: [...AZURE.models, entry('x1', 'One'), entry('x2', 'Two')]}
    renderPicker({providers: [CLAUDE, many]})
    await openPicker(user)
    const popup = dialog()
    expect(within(popup).getByRole('combobox', {name: 'Search models'})).toHaveFocus()
    await user.tab({shift: true})
    expect(within(popup).getByRole('combobox', {name: 'Provider'})).toHaveFocus()
    await user.tab()
    await user.tab()
    expect(within(popup).getByRole('listbox')).toHaveFocus()
    await user.tab()
    expect(within(popup).getByRole('button', {name: 'Manage models…'})).toHaveFocus()
    await user.tab()
    expect(screen.getByRole('button', {name: 'after the picker'})).toHaveFocus()
    expect(screen.queryByRole('dialog')).toBeNull()
  })

  it('closes on a press outside', async () => {
    const user = userEvent.setup()
    renderPicker()
    await openPicker(user)
    await user.click(screen.getByRole('button', {name: 'after the picker'}))
    expect(screen.queryByRole('dialog')).toBeNull()
  })

  it('the Provider select filters the list and pins nothing', async () => {
    const user = userEvent.setup()
    const {props} = renderPicker({providers: [CLAUDE, AZURE]})
    await openPicker(user)
    const select = within(dialog()).getByRole('combobox', {name: 'Provider'})
    expect(select).toHaveValue('azure-foundry')
    await user.selectOptions(select, 'claude')
    expect(optionTexts()).toEqual([
      expect.stringMatching(/^Claude Sonnet 4\.6/),
      expect.stringMatching(/^Claude Haiku 4\.5/),
    ])
    expect(props.onSelect).not.toHaveBeenCalled()
    await user.click(option(/^Claude Sonnet 4\.6/))
    expect(props.onSelect).toHaveBeenCalledWith('claude', SONNET)
  })

  it('Manage models… returns focus to the trigger, then opens Settings at the models', async () => {
    const user = userEvent.setup()
    let focusedWhenOpened: Element | null = null
    const openSettings = vi.fn(() => {
      focusedWhenOpened = document.activeElement
    })
    useUIStore.setState({openSettings})
    renderPicker()
    await openPicker(user)
    await user.click(within(dialog()).getByRole('button', {name: 'Manage models…'}))
    expect(openSettings).toHaveBeenCalledWith('accounts', 'provider-azure-foundry')
    expect(focusedWhenOpened).toBe(trigger())
    expect(screen.queryByRole('dialog')).toBeNull()
  })
})

describe('ModelPicker empty states and prompts', () => {
  it('Azure without chat models: says so, with a way to add some', async () => {
    const user = userEvent.setup()
    const openSettings = vi.fn()
    useUIStore.setState({openSettings})
    renderPicker({providers: [{...AZURE, models: [EMBED]}], model: '', label: '', modelDetail: undefined})
    await openPicker(user)
    expect(within(dialog()).getByText('No Azure AI Foundry models yet…')).toBeInTheDocument()
    expect(within(dialog()).queryByRole('listbox')).toBeNull()
    await user.click(within(dialog()).getByRole('button', {name: 'Add models…'}))
    expect(openSettings).toHaveBeenCalledWith('accounts', 'provider-azure-foundry')
  })

  // C3: with nothing to pick there is no search box or list to take focus;
  // it stayed outside, so Escape reached the chat's own (stop the reply,
  // leave focus mode) and focus leaving never closed the popup.
  it('with nothing to pick, focus still lands in the popup; Escape closes it and never reaches the chat', async () => {
    const user = userEvent.setup()
    renderPicker({providers: [{...AZURE, models: [EMBED]}], model: '', label: '', modelDetail: undefined})
    const reachedDocument: string[] = []
    const chatEscape = (e: KeyboardEvent) => reachedDocument.push(e.key)
    document.addEventListener('keydown', chatEscape)
    try {
      await openPicker(user)
      expect(within(dialog()).getByRole('button', {name: 'Add models…'})).toHaveFocus()
      await user.keyboard('{Escape}')
      expect(screen.queryByRole('dialog')).toBeNull()
      expect(trigger()).toHaveFocus()
      expect(reachedDocument).not.toContain('Escape')

      // Focus leaving closes it too.
      await openPicker(user)
      await user.tab()
      await user.tab()
      expect(screen.queryByRole('dialog')).toBeNull()
    } finally {
      document.removeEventListener('keydown', chatEscape)
    }
  })

  it('opened from a notice’s “Choose model” on a provider with nothing to pick, takes focus into the popup', async () => {
    const user = userEvent.setup()
    render(
      <>
        <ModelPicker
          providers={[{...CLAUDE, models: []}, AZURE]}
          provider="openai"
          model=""
          label=""
          defaults={{}}
          onSelect={vi.fn()}
        />
        <button type="button" onClick={() => useChatStore.getState().openModelPicker()}>
          Choose model
        </button>
      </>,
    )
    await user.click(screen.getByRole('button', {name: 'Choose model'}))
    // The thread's provider isn't listed: the first one shows — with no
    // models, so the Provider select takes focus.
    expect(within(dialog()).getByRole('combobox', {name: 'Provider'})).toHaveFocus()
    await user.keyboard('{Escape}')
    expect(screen.queryByRole('dialog')).toBeNull()
  })

  it('a provider that lists nothing (a local server that is down): focus on “Manage models…”', async () => {
    const user = userEvent.setup()
    renderPicker({providers: [{id: 'local', name: 'Local model', models: []}], provider: 'local', model: '', label: ''})
    await openPicker(user)
    expect(within(dialog()).getByText('Local model lists no models.')).toBeInTheDocument()
    expect(within(dialog()).getByRole('button', {name: 'Manage models…'})).toHaveFocus()
  })

  it("offers the Review prompt when the project has models the user hasn't added", async () => {
    const user = userEvent.setup()
    const openSettings = vi.fn()
    useUIStore.setState({openSettings})
    const {rerender, props} = renderPicker()
    await openPicker(user)
    expect(within(dialog()).queryByText("Your project has models you haven't added")).toBeNull()
    await user.keyboard('{Escape}')

    rerender(
      <>
        <ModelPicker {...props} reviewPrompt />
        <button type="button">after the picker</button>
      </>,
    )
    await openPicker(user)
    expect(within(dialog()).getByText("Your project has models you haven't added")).toBeInTheDocument()
    await user.click(within(dialog()).getByRole('button', {name: 'Review'}))
    expect(openSettings).toHaveBeenCalledWith('accounts', 'provider-azure-foundry')
  })
})
