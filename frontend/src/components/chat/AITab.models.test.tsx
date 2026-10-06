import {describe, it, expect, vi, beforeEach, afterEach} from 'vitest'
import {render, screen, waitFor, act, within} from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import AITab from './AITab'
import ChatStreamProvider from './ChatStreamProvider'
import ChatPanelShell from './ChatPanelShell'
import {useUIStore} from '@/stores/uiStore'
import {chatApi, providersApi, flowApi, ApiError} from '@/api'
import {useFlowStore} from '@/stores/flowStore'
import {useChatStore, threadModelOf, type ChatThread} from '@/stores/chatStore'
import {useSettingsStore} from '@/stores/settingsStore'
import {ToastProvider} from '@/components/shared/Toast'
import {ConfirmProvider} from '@/components/shared/ConfirmDialog'
import type {ModelDetail, ProviderInfo, ChatMessage} from '@/types'
import type {ReactNode} from 'react'

// Per-thread model memory in the chat panel: the header's picker, the model
// notice (announced through the panel's status region), the welcome line,
// the composer's guidance, and a send refused because the model is gone.

vi.mock('react-virtuoso', () => ({
  Virtuoso: ({
    data = [],
    itemContent,
    components,
    context,
  }: {
    data?: unknown[]
    itemContent: (i: number, item: unknown) => ReactNode
    components?: {Footer?: (props: {context?: unknown}) => ReactNode}
    context?: unknown
  }) => (
    <div>
      {data.map((item, i) => (
        <div key={i}>{itemContent(i, item)}</div>
      ))}
      {components?.Footer ? <components.Footer context={context} /> : null}
    </div>
  ),
}))

vi.mock('@/hooks/useMediaQuery', () => ({useIsDesktop: () => true, useMediaQuery: () => true}))

// The event stream is "open", so a send reaches the create request.
vi.mock('@/api/client', async importOriginal => ({
  ...(await importOriginal<typeof import('@/api/client')>()),
  subscribeToEvents: vi.fn(async () => () => {}),
  subscribeConnectionState: vi.fn((cb: (state: string) => void) => {
    cb('open')
    return () => {}
  }),
  getEventConnectionState: vi.fn(() => 'open'),
}))

vi.mock('@/api', () => ({
  ApiError: class ApiError extends Error {
    constructor(
      message: string,
      public status = 0,
      public code: string | null = null,
    ) {
      super(message)
    }
  },
  chatApi: {
    getSuggestedPrompts: vi.fn(),
    streamChatMessage: vi.fn(),
    getConversation: vi.fn(),
    saveConversation: vi.fn(),
    clearConversation: vi.fn(),
    cancelStream: vi.fn(),
    previewContext: vi.fn(),
    respondFixDecision: vi.fn(),
  },
  providersApi: {listProviders: vi.fn()},
  flowApi: {getSourceFiles: vi.fn()},
}))

const GPT: ModelDetail = {
  id: 'm/gpt4100000',
  displayName: 'GPT-4.1',
  contextLimit: 1_000_000,
  inputCostPerM: 0,
  outputCostPerM: 0,
  kind: 'chat',
  purpose: 'general',
  isDefault: true,
}
const DOCS: ModelDetail = {
  id: 'm/docs000000',
  displayName: 'Docs writer',
  contextLimit: 200_000,
  inputCostPerM: 0,
  outputCostPerM: 0,
  kind: 'claude',
  purpose: 'documentation',
  description: 'Writes flow documentation',
}
const azure = (models: ModelDetail[]) =>
  ({
    id: 'azure-foundry',
    name: 'Azure AI Foundry',
    configured: true,
    authType: 'api_key',
    defaultModel: models.find(m => m.kind !== 'embedding')?.id ?? '',
    models,
  }) as unknown as ProviderInfo

const T1 = 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa'

function thread(id: string, title: string, extra: Partial<ChatThread> = {}): ChatThread {
  return {
    id,
    flowId: 'f1',
    title,
    createdAt: '2026-09-30T00:00:00Z',
    contextBlockId: null,
    selectedSourceFiles: [],
    tokensIn: 0,
    tokensOut: 0,
    ...extra,
  }
}

function seed(extra: Partial<ChatThread> = {}, streaming = false) {
  useFlowStore.setState({document: {id: 'f1', name: 'Flow', subflows: []} as never})
  useChatStore.setState({
    activeThreadId: T1,
    threads: [thread(T1, 'Docs pass', extra)],
    conversations: new Map([[T1, streaming ? [{id: 'u1', role: 'user' as const, content: 'hi', timestamp: 't'}] : []]]),
    historyStatus: {[T1]: {state: 'loaded', token: 1}},
    streams: streaming
      ? {
          [T1]: {
            streamId: 'pending',
            messageId: 'm2',
            text: '',
            isThinking: false,
            tokens: 0,
            toolStatus: null,
            toolCalls: [],
            fixProposals: [],
          },
        }
      : {},
  })
}

function renderPanel({focusShell = false} = {}) {
  const tab = focusShell ? (
    <ChatPanelShell>
      <AITab />
    </ChatPanelShell>
  ) : (
    <AITab />
  )
  return render(
    <ToastProvider>
      <ConfirmProvider>
        <ChatStreamProvider>{tab}</ChatStreamProvider>
      </ConfirmProvider>
    </ToastProvider>,
  )
}

const picker = () => screen.getByRole('button', {name: /^Model for this chat:/})
const notice = () => document.querySelector('[data-model-notice]')

const initialChatState = useChatStore.getState()
const initialFlowState = useFlowStore.getState()

beforeEach(() => {
  vi.clearAllMocks()
  useFlowStore.setState(initialFlowState, true)
  useChatStore.setState(initialChatState, true)
  useUIStore.setState({chatFocus: false})
  vi.mocked(chatApi.getSuggestedPrompts).mockResolvedValue([])
  vi.mocked(chatApi.getConversation).mockResolvedValue({messages: []})
  vi.mocked(chatApi.cancelStream).mockResolvedValue(undefined)
  vi.mocked(chatApi.saveConversation).mockResolvedValue(undefined)
  vi.mocked(flowApi.getSourceFiles).mockResolvedValue([])
  vi.mocked(providersApi.listProviders).mockResolvedValue([azure([GPT, DOCS])])
  const s = useSettingsStore.getState().settings
  useSettingsStore.setState({
    settings: {
      ...s,
      ai: {
        ...s.ai,
        activeProvider: 'azure-foundry',
        providers: {...s.ai.providers, 'azure-foundry': {...s.ai.providers['azure-foundry'], defaultModel: ''}},
      },
    },
  })
})

describe('AITab per-thread model', () => {
  it("names the thread's model in the header and the welcome line", async () => {
    seed({provider: 'azure-foundry', model: DOCS.id, modelLabel: 'Docs writer'})
    renderPanel()
    await waitFor(() =>
      expect(picker()).toHaveAccessibleName('Model for this chat: Docs writer, Documentation, Azure AI Foundry'),
    )
    expect(screen.getByText('This chat uses Docs writer · Documentation')).toBeInTheDocument()
  })

  it("starts a new thread on the Default for new chats; each tab's tooltip names its own model", async () => {
    const user = userEvent.setup()
    seed({provider: 'azure-foundry', model: DOCS.id, modelLabel: 'Docs writer'})
    renderPanel()
    await waitFor(() => expect(picker()).toHaveAccessibleName(/Docs writer/))

    await user.click(screen.getByRole('button', {name: 'New chat thread'}))
    await waitFor(() =>
      expect(picker()).toHaveAccessibleName('Model for this chat: GPT-4.1, General, Azure AI Foundry'),
    )
    expect(screen.getByTitle('Docs pass — Docs writer · Documentation')).toBeInTheDocument()
    expect(screen.getByTitle('New chat — GPT-4.1 · General')).toBeInTheDocument()
  })

  it('announces a removed model through the status region; Choose model opens the picker, Dismiss keeps the stand-in', async () => {
    const user = userEvent.setup()
    vi.mocked(providersApi.listProviders).mockResolvedValue([azure([GPT])])
    seed({provider: 'azure-foundry', model: DOCS.id, modelLabel: 'Docs writer'})
    renderPanel()

    const text = '“Docs writer” is no longer available. This chat now uses “GPT-4.1”.'
    await waitFor(() => expect(notice()).toHaveTextContent(text))
    // Announced once, by the always-mounted region — the row is not live.
    const region = screen.getAllByRole('status').find(el => el.textContent === text)
    expect(region).toHaveClass('sr-only')
    expect(notice()!.closest('[aria-live], [role="status"], [role="alert"]')).toBeNull()

    await user.click(within(notice() as HTMLElement).getByRole('button', {name: 'Choose model'}))
    expect(screen.getByRole('dialog', {name: 'Choose a model for this chat'})).toBeInTheDocument()
    await user.keyboard('{Escape}')
    expect(screen.queryByRole('dialog', {name: 'Choose a model for this chat'})).toBeNull()

    await user.click(within(notice() as HTMLElement).getByRole('button', {name: 'Dismiss'}))
    expect(threadModelOf(useChatStore.getState().threads[0]!)).toEqual({
      provider: 'azure-foundry',
      model: GPT.id,
      modelLabel: 'GPT-4.1',
    })
    expect(notice()).toBeNull()
    expect(picker()).toHaveAccessibleName(/GPT-4\.1/)
  })

  it('the composer says Azure has no models to chat with, instead of sending', async () => {
    const user = userEvent.setup()
    vi.mocked(providersApi.listProviders).mockResolvedValue([azure([])])
    seed()
    renderPanel()
    await waitFor(() =>
      expect(screen.getByText(/^No Azure AI Foundry models yet\. Add the deployments/)).toBeInTheDocument(),
    )
    const composer = screen.getByRole('textbox', {name: 'Message the assistant'})
    await user.type(composer, 'hello{Enter}')
    expect(chatApi.streamChatMessage).not.toHaveBeenCalled()
    expect(composer).toHaveValue('hello')
    const openSettings = vi.fn()
    const original = useUIStore.getState().openSettings
    useUIStore.setState({openSettings})
    await user.click(screen.getByRole('button', {name: 'Add models…'}))
    expect(openSettings).toHaveBeenCalledWith('accounts', 'provider-azure-foundry')
    useUIStore.setState({openSettings: original})
  })

  it('a send refused with MODEL_NOT_AVAILABLE: the text returns, the listing refreshes, the notice shows', async () => {
    const user = userEvent.setup()
    // The model is removed after the picker listed it.
    vi.mocked(providersApi.listProviders)
      .mockResolvedValueOnce([azure([GPT, DOCS])])
      .mockResolvedValue([azure([GPT])])
    vi.mocked(chatApi.streamChatMessage).mockRejectedValue(
      new ApiError('model m/docs000000 is not available', 400, 'MODEL_NOT_AVAILABLE'),
    )
    seed({provider: 'azure-foundry', model: DOCS.id, modelLabel: 'Docs writer'})
    renderPanel()
    await waitFor(() => expect(picker()).toHaveAccessibleName(/Docs writer/))

    const composer = screen.getByRole('textbox', {name: 'Message the assistant'})
    await user.type(composer, 'Document this flow{Enter}')

    await waitFor(() => expect(chatApi.streamChatMessage).toHaveBeenCalledTimes(1))
    expect(vi.mocked(chatApi.streamChatMessage).mock.calls[0]![0].model).toBe(DOCS.id)
    // The text is back in the composer; no orphan question, no error bubble.
    await waitFor(() => expect(composer).toHaveValue('Document this flow'))
    expect(useChatStore.getState().getMessages(T1)).toHaveLength(0)
    // The listing is refetched past the GET micro-cache…
    await waitFor(() => expect(providersApi.listProviders).toHaveBeenLastCalledWith(undefined, {noDedup: true}))
    // …and the thread's notice explains it.
    await waitFor(() =>
      expect(notice()).toHaveTextContent('“Docs writer” is no longer available. This chat now uses “GPT-4.1”.'),
    )
  })

  it('Escape in the picker neither stops the reply nor leaves focus mode', async () => {
    const user = userEvent.setup()
    seed({provider: 'azure-foundry', model: DOCS.id, modelLabel: 'Docs writer'}, true)
    useUIStore.setState({chatFocus: true, inspectorTab: 'ai', inspectorCollapsed: false})
    renderPanel({focusShell: true})
    await waitFor(() => expect(picker()).toHaveAccessibleName(/Docs writer/))

    await user.click(picker())
    expect(screen.getByRole('dialog', {name: 'Choose a model for this chat'})).toBeInTheDocument()
    await user.keyboard('{Escape}')

    expect(screen.queryByRole('dialog', {name: 'Choose a model for this chat'})).toBeNull()
    expect(picker()).toHaveFocus()
    expect(useChatStore.getState().streams[T1]).toBeDefined()
    expect(useUIStore.getState().chatFocus).toBe(true)
    expect(chatApi.cancelStream).not.toHaveBeenCalled()
  })

  it('a header pick pins only the active thread and writes no settings', async () => {
    const user = userEvent.setup()
    const original = useSettingsStore.getState().updateProvider
    const updateProvider = vi.fn(async () => true)
    useSettingsStore.setState({updateProvider})
    try {
      seed()
      renderPanel()
      await waitFor(() => expect(picker()).toHaveAccessibleName(/GPT-4\.1/))
      await user.click(picker())
      await user.click(screen.getByRole('option', {name: /^Docs writer/}))
      expect(threadModelOf(useChatStore.getState().threads[0]!)).toMatchObject({model: DOCS.id})
      expect(picker()).toHaveAccessibleName(/Docs writer/)
      expect(updateProvider).not.toHaveBeenCalled()
      expect(useSettingsStore.getState().settings.ai.providers['azure-foundry'].defaultModel).toBe('')
    } finally {
      useSettingsStore.setState({updateProvider: original})
    }
  })
})

describe('AITab per-thread model: review fixes', () => {
  const LEGACY: ModelDetail = {
    id: 'm/legacy0000',
    displayName: 'Old GPT',
    contextLimit: 128_000,
    inputCostPerM: 0,
    outputCostPerM: 0,
    kind: 'chat',
    purpose: 'general',
    aliases: ['gpt-4.1'],
    status: 'needs_credentials',
  }
  const legacyHistory = [
    {id: 'u0', role: 'user' as const, content: 'q', timestamp: '2026-01-01T00:00:00Z'},
    {
      id: 'a0',
      role: 'assistant' as const,
      content: 'a',
      timestamp: '2026-01-01T00:00:01Z',
      provider: 'azure-foundry' as const,
      model: 'gpt-4.1',
    },
  ]
  const pinOfT1 = () => threadModelOf(useChatStore.getState().threads.find(th => th.id === T1)!)

  // Unpinned, the stand-in's first reply became the thread's last reply,
  // and legacy inference adopted the stand-in for good.
  it("a legacy thread whose model is only unavailable keeps it: a reply from the stand-in doesn't move it", async () => {
    const user = userEvent.setup()
    vi.mocked(providersApi.listProviders).mockResolvedValue([azure([GPT, LEGACY])])
    vi.mocked(chatApi.streamChatMessage).mockResolvedValue('sid')
    seed()
    useChatStore.setState({conversations: new Map([[T1, legacyHistory]])})
    renderPanel()
    await waitFor(() => expect(notice()).toHaveTextContent(/unavailable until its resource's credentials/))
    // Pinned to its own model at once (through the alias), not to the stand-in.
    await waitFor(() => expect(pinOfT1()).toMatchObject({provider: 'azure-foundry', model: LEGACY.id}))

    const composer = screen.getByRole('textbox', {name: 'Message the assistant'})
    await user.type(composer, 'next{Enter}')
    await waitFor(() => expect(chatApi.streamChatMessage).toHaveBeenCalledTimes(1))
    expect(vi.mocked(chatApi.streamChatMessage).mock.calls[0]![0].model).toBe(GPT.id)

    // The stand-in's reply lands (what the done commit appends).
    act(() => {
      useChatStore.getState().endStream(T1)
      useChatStore.getState().appendMessage(T1, {
        id: 'a1',
        role: 'assistant',
        content: 'reply',
        timestamp: '2026-01-01T00:00:03Z',
        provider: 'azure-foundry',
        model: GPT.id,
        modelLabel: 'GPT-4.1',
      })
    })
    await act(async () => {})
    expect(pinOfT1()).toMatchObject({model: LEGACY.id})
    expect(notice()).toHaveTextContent(/unavailable until its resource's credentials/)

    // Once its credentials are back, the thread is on its own model again.
    vi.mocked(providersApi.listProviders).mockResolvedValue([azure([GPT, {...LEGACY, status: 'ready'}])])
    act(() => useChatStore.getState().bumpProviderEpoch())
    await waitFor(() => expect(picker()).toHaveAccessibleName(/^Model for this chat: Old GPT/))
    expect(notice()).toBeNull()
  })

  // Azure configured with no models (its legacy import is pending) read
  // as "every model removed", and the thread moved to another model.
  it('a thread keeps its Azure model while Azure lists nothing to chat with — no notice, no move', async () => {
    vi.mocked(providersApi.listProviders).mockResolvedValue([azure([])])
    seed({provider: 'azure-foundry', model: DOCS.id, modelLabel: 'Docs writer'})
    renderPanel()
    await waitFor(() => expect(picker()).toHaveAccessibleName('Model for this chat: Docs writer, Azure AI Foundry'))
    expect(notice()).toBeNull()
    expect(pinOfT1()).toEqual({provider: 'azure-foundry', model: DOCS.id, modelLabel: 'Docs writer'})
    // The composer's guidance is for a thread with no model at all.
    expect(screen.queryByText(/^No Azure AI Foundry models yet\. Add the deployments/)).toBeNull()
  })

  // With nothing to pick, focus never entered the popup, so Escape went
  // to the chat (leaving focus mode) and the popup stayed.
  it('the picker with nothing to pick takes focus; Escape closes it, not focus mode', async () => {
    const user = userEvent.setup()
    vi.mocked(providersApi.listProviders).mockResolvedValue([azure([])])
    seed()
    useUIStore.setState({chatFocus: true, inspectorTab: 'ai', inspectorCollapsed: false})
    renderPanel({focusShell: true})
    await waitFor(() => expect(screen.getByText(/^No Azure AI Foundry models yet\. Add/)).toBeInTheDocument())
    await user.click(picker())
    const dlg = screen.getByRole('dialog', {name: 'Choose a model for this chat'})
    expect(dlg.contains(document.activeElement)).toBe(true)
    await user.keyboard('{Escape}')
    expect(screen.queryByRole('dialog', {name: 'Choose a model for this chat'})).toBeNull()
    expect(picker()).toHaveFocus()
    expect(useUIStore.getState().chatFocus).toBe(true)
  })

  // A refused send whose listing refetch failed returned the text
  // silently — no notice (it waited for a listing), no toast.
  it('a send refused for its model says so at once, and the notice shows even when the refetch fails', async () => {
    const user = userEvent.setup()
    vi.mocked(providersApi.listProviders)
      .mockResolvedValueOnce([azure([GPT, DOCS])])
      .mockRejectedValue(new Error('network'))
    vi.mocked(chatApi.streamChatMessage).mockRejectedValue(new ApiError('not available', 400, 'MODEL_NOT_AVAILABLE'))
    seed({provider: 'azure-foundry', model: DOCS.id, modelLabel: 'Docs writer'})
    renderPanel()
    await waitFor(() => expect(picker()).toHaveAccessibleName(/Docs writer/))
    const composer = screen.getByRole('textbox', {name: 'Message the assistant'})
    await user.type(composer, 'Document this flow{Enter}')
    await waitFor(() => expect(composer).toHaveValue('Document this flow'))
    expect(await screen.findByText('This model is no longer available')).toBeInTheDocument()
    await waitFor(() =>
      expect(notice()).toHaveTextContent(
        "“Docs writer” isn't available any more. Choose another model to send your message.",
      ),
    )
  })

  // The guidance held the composer only; a suggested prompt (or a
  // template, or Regenerate) still went out with model ''.
  it('a suggested prompt sends nothing while Azure has no model to chat with, and says why', async () => {
    const user = userEvent.setup()
    vi.mocked(chatApi.getSuggestedPrompts).mockResolvedValue(['Explain this flow'])
    vi.mocked(chatApi.streamChatMessage).mockResolvedValue('sid')
    vi.mocked(providersApi.listProviders).mockResolvedValue([azure([])])
    seed()
    renderPanel()
    const reason = "No Azure AI Foundry models yet. Add the deployments you want to chat with, and they'll appear here."
    await waitFor(() => expect(screen.getAllByText(reason).length).toBeGreaterThan(0))
    const chip = await screen.findByRole('button', {name: 'Explain this flow'})
    expect(chip).toHaveAttribute('aria-disabled', 'true')
    expect(chip).toHaveAccessibleDescription(reason)
    await user.click(chip)
    await user.click(screen.getByRole('button', {name: 'Templates'}))
    const template = screen.getByRole('button', {name: 'Explain entire flow'})
    expect(template).toHaveAttribute('aria-disabled', 'true')
    await user.click(template)
    await act(async () => {})
    expect(chatApi.streamChatMessage).not.toHaveBeenCalled()
    expect(useChatStore.getState().getMessages(T1)).toHaveLength(0)
  })

  // The status region sat in a tree that rendered only once the
  // providers were checked, so it was inserted already holding a notice
  // raised by that first listing — and a region inserted with its text is
  // not announced.
  it('the status region is on screen, empty, before the first listing — the notice then fills it', async () => {
    let resolveListing: (ps: ProviderInfo[]) => void = () => {}
    vi.mocked(providersApi.listProviders).mockImplementation(
      () => new Promise<ProviderInfo[]>(resolve => (resolveListing = resolve)),
    )
    seed({provider: 'azure-foundry', model: DOCS.id, modelLabel: 'Docs writer'})
    const insertedWithText: string[] = []
    const observer = new MutationObserver(records => {
      for (const r of records)
        r.addedNodes.forEach(n => {
          if (!(n instanceof HTMLElement)) return
          for (const el of [n, ...Array.from(n.querySelectorAll('*'))]) {
            if (el.getAttribute('role') === 'status' && el.classList.contains('sr-only') && el.textContent) {
              insertedWithText.push(el.textContent)
            }
          }
        })
    })
    observer.observe(document.body, {childList: true, subtree: true})
    try {
      renderPanel()
      const region = document.querySelector('div[role="status"].sr-only')
      expect(region).not.toBeNull()
      expect(region).toBeEmptyDOMElement()

      await act(async () => resolveListing([azure([GPT])]))
      const text = '“Docs writer” is no longer available. This chat now uses “GPT-4.1”.'
      await waitFor(() => expect(notice()).toHaveTextContent(text))
      // The same element — mounted before — now holds the text.
      expect(region).toHaveTextContent(text)
      expect(region?.isConnected).toBe(true)
      expect(insertedWithText).toEqual([])
    } finally {
      observer.disconnect()
    }
  })

  // Dismiss unmounted the focused button and focus fell to <body>.
  it('Dismiss moves focus to the model picker, which names the model the thread now uses', async () => {
    const user = userEvent.setup()
    vi.mocked(providersApi.listProviders).mockResolvedValue([azure([GPT])])
    seed({provider: 'azure-foundry', model: DOCS.id, modelLabel: 'Docs writer'})
    renderPanel()
    await waitFor(() => expect(notice()).not.toBeNull())
    within(notice() as HTMLElement)
      .getByRole('button', {name: 'Dismiss'})
      .focus()
    await user.keyboard('{Enter}')
    await waitFor(() => expect(notice()).toBeNull())
    expect(picker()).toHaveFocus()
    expect(picker()).toHaveAccessibleName(/GPT-4\.1/)
  })

  // Sent while an unpinned thread's history loaded, a turn pinned the
  // new-chat default, and legacy inference never ran on that history.
  it("a send while the thread's history loads pins nothing; the history's own model then does", async () => {
    const user = userEvent.setup()
    let resolveLoad: (load: {messages: typeof legacyHistory}) => void = () => {}
    vi.mocked(chatApi.getConversation).mockImplementation(() => new Promise(resolve => (resolveLoad = resolve)))
    vi.mocked(chatApi.streamChatMessage).mockResolvedValue('sid')
    seed()
    useChatStore.setState({historyStatus: {}, conversations: new Map()})
    renderPanel()
    await waitFor(() => expect(picker()).toHaveAccessibleName(/GPT-4\.1/))

    const composer = screen.getByRole('textbox', {name: 'Message the assistant'})
    await user.type(composer, 'hello{Enter}')
    await waitFor(() => expect(chatApi.streamChatMessage).toHaveBeenCalledTimes(1))
    expect(pinOfT1()).toBeNull()

    const olderDocsReply = [
      legacyHistory[0],
      {...legacyHistory[1], model: DOCS.id, modelLabel: 'Docs writer'},
    ] as typeof legacyHistory
    await act(async () => resolveLoad({messages: olderDocsReply}))
    await waitFor(() =>
      expect(pinOfT1()).toEqual({provider: 'azure-foundry', model: DOCS.id, modelLabel: 'Docs writer'}),
    )
  })

  // An inactive thread's tab tooltip ignored its own history.
  it("an inactive legacy thread's tab tooltip names its own model", async () => {
    const T2 = 'bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb'
    seed({provider: 'azure-foundry', model: GPT.id, modelLabel: 'GPT-4.1'})
    const t2History: ChatMessage[] = [
      legacyHistory[0]!,
      {...legacyHistory[1]!, model: DOCS.id, modelLabel: 'Docs writer'},
    ]
    const t2Entry: [string, ChatMessage[]] = [T2, t2History]
    useChatStore.setState(s => ({
      threads: [...s.threads, thread(T2, 'Earlier docs')],
      conversations: new Map([...s.conversations, t2Entry]),
    }))
    renderPanel()
    await waitFor(() => expect(screen.getByTitle('Earlier docs — Docs writer · Documentation')).toBeInTheDocument())
    expect(screen.getByTitle('Docs pass — GPT-4.1 · General')).toBeInTheDocument()
  })
})

// Keeps act() warnings from a late provider refetch out of other files.
afterEach(async () => {
  await act(async () => {})
})
