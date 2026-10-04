import {describe, it, expect, beforeEach, vi} from 'vitest'
import {render, screen, fireEvent} from '@testing-library/react'
import MessageBubble from './MessageBubble'
import {useFlowStore} from '@/stores/flowStore'
import {useUIStore} from '@/stores/uiStore'
import {useAnalysisStore} from '@/stores/analysisStore'
import {useChatStore} from '@/stores/chatStore'
import type {ChatMessage} from '@/types'

function msg(partial: Partial<ChatMessage>): ChatMessage {
  return {
    id: 'm1',
    role: 'assistant',
    content: '',
    timestamp: new Date().toISOString(),
    ...partial,
  } as ChatMessage
}

beforeEach(() => {
  vi.restoreAllMocks()
})

describe('MessageBubble — interrupted state', () => {
  it('shows a Stopped affordance for an interrupted message', () => {
    render(<MessageBubble message={msg({content: 'partial answer', finishReason: 'interrupted'})} />)
    expect(screen.getByText('Stopped')).toBeInTheDocument()
    expect(screen.getByText('partial answer')).toBeInTheDocument()
  })

  it('does not show Stopped for a normally completed message', () => {
    render(<MessageBubble message={msg({content: 'done', finishReason: 'stop'})} />)
    expect(screen.queryByText('Stopped')).not.toBeInTheDocument()
  })
})

describe('MessageBubble — chat→app navigation', () => {
  it('navigates to the matching subflow when a mention pill is clicked', () => {
    // F1-followup: patch INDIVIDUAL actions on the REAL stores (not a
    // full-store getState mock that silently keeps passing when the
    // component starts reading other state).
    const navigateToSourceFile = vi.fn(() => true)
    const setMainPaneView = vi.fn()
    const prevFlow = useFlowStore.getState()
    const prevUI = useUIStore.getState()
    useFlowStore.setState({navigateToSourceFile} as never)
    useUIStore.setState({setMainPaneView} as never)

    render(<MessageBubble message={msg({role: 'user', content: 'look at @Login.txt please'})} />)
    fireEvent.click(screen.getByRole('button', {name: /Go to Login.txt/}))

    expect(navigateToSourceFile).toHaveBeenCalledWith('Login.txt')
    expect(setMainPaneView).toHaveBeenCalledWith('graph')
    useFlowStore.setState({navigateToSourceFile: prevFlow.navigateToSourceFile} as never)
    useUIStore.setState({setMainPaneView: prevUI.setMainPaneView} as never)
  })

  it('intercepts a block: markdown link and jumps to the block instead of navigating', () => {
    const navigateToBlock = vi.fn()
    const setMainPaneView = vi.fn()
    const setInspectorTab = vi.fn()
    vi.spyOn(useFlowStore, 'getState').mockReturnValue({navigateToBlock} as never)
    vi.spyOn(useUIStore, 'getState').mockReturnValue({setMainPaneView, setInspectorTab} as never)

    render(<MessageBubble message={msg({content: 'See [Run SQL](block:blk-42) for details.'})} />)
    fireEvent.click(screen.getByRole('button', {name: 'Run SQL'}))

    expect(navigateToBlock).toHaveBeenCalledWith('blk-42')
    expect(setInspectorTab).toHaveBeenCalledWith('details')
  })

  it('intercepts a finding: link and focuses the finding in the findings tab', () => {
    const setInspectorTab = vi.fn()
    const setFocusedFinding = vi.fn()
    vi.spyOn(useUIStore, 'getState').mockReturnValue({setInspectorTab} as never)
    vi.spyOn(useAnalysisStore, 'getState').mockReturnValue({setFocusedFinding} as never)

    render(<MessageBubble message={msg({content: 'See [the secret](finding:hardcoded-credential:blk-1).'})} />)
    fireEvent.click(screen.getByRole('button', {name: 'the secret'}))

    expect(setInspectorTab).toHaveBeenCalledWith('findings')
    expect(setFocusedFinding).toHaveBeenCalledWith('hardcoded-credential:blk-1')
  })

  it('renders a normal external link as an anchor (no block scheme)', () => {
    render(<MessageBubble message={msg({content: 'docs at [here](https://example.com).'})} />)
    const link = screen.getByRole('link', {name: 'here'})
    expect(link).toHaveAttribute('href', 'https://example.com')
    expect(link).toHaveAttribute('target', '_blank')
  })
})

describe('MessageBubble — reading layout', () => {
  it('wraps a markdown table in its own horizontal scroll container', () => {
    const table = '| a | b |\n| --- | --- |\n| 1 | 2 |'
    const {container} = render(<MessageBubble message={msg({content: table})} />)
    const wrap = container.querySelector('.chat-table-wrap')
    expect(wrap).not.toBeNull()
    expect(wrap!.querySelector('table')).not.toBeNull()
  })

  it('tints the user bubble with an opacity step Tailwind generates', () => {
    const {container} = render(<MessageBubble message={msg({role: 'user', content: 'hi'})} />)
    expect(container.querySelector('.bg-brand-500\\/10')).not.toBeNull()
  })
})

describe('MessageBubble — served model', () => {
  it('names the model that answered when it differs from the selected one', () => {
    render(<MessageBubble message={msg({content: 'hi', model: 'my-deployment', servedModel: 'gpt-4o-2024-08-06'})} />)
    expect(screen.getByText('· my-deployment')).toBeInTheDocument()
    expect(screen.getByTitle('Answered by gpt-4o-2024-08-06')).toHaveTextContent('· gpt-4o-2024-08-06')
    // Named for screen readers too, not only in the mouse tooltip.
    expect(screen.getByRole('img', {name: 'Answered by gpt-4o-2024-08-06'})).toBeInTheDocument()
  })

  it('does not repeat the model when the served one is the same, and never on user turns', () => {
    render(<MessageBubble message={msg({content: 'hi', model: 'gpt-4o', servedModel: 'gpt-4o'})} />)
    expect(screen.getAllByText('· gpt-4o')).toHaveLength(1)
    expect(screen.queryByTitle(/Answered by/)).toBeNull()

    render(<MessageBubble message={msg({id: 'm2', role: 'user', content: 'q', model: 'a', servedModel: 'b'})} />)
    expect(screen.queryByText('· b')).toBeNull()
  })
})

describe('MessageBubble — provider errors', () => {
  it('offers "Open settings" on a provider error bubble and opens the section that fixes it', () => {
    const openSettings = vi.fn()
    const original = useUIStore.getState().openSettings
    useUIStore.setState({openSettings})
    render(
      <MessageBubble
        message={msg({content: '**Denied** — detail', finishReason: 'error', errorCode: 'PROVIDER_OUTPUT_LIMIT'})}
      />,
    )
    fireEvent.click(screen.getByRole('button', {name: 'Open settings'}))
    expect(openSettings).toHaveBeenCalledWith('behavior')
    useUIStore.setState({openSettings: original})
  })

  it('offers no settings action on other errors', () => {
    render(<MessageBubble message={msg({content: 'x', finishReason: 'error', errorCode: 'STREAM_TIMEOUT'})} />)
    expect(screen.queryByRole('button', {name: 'Open settings'})).toBeNull()
  })

  // A failure on the organisation's shared credential also offers the
  // Organizations section — the member cannot fix it under their own
  // Accounts.
  it('a shared-credential auth failure also offers the organisation section', () => {
    const openSettings = vi.fn()
    const original = useUIStore.getState().openSettings
    useUIStore.setState({openSettings})
    render(
      <MessageBubble
        message={msg({content: 'ask your admin', finishReason: 'error', errorCode: 'PROVIDER_AUTH', errorShared: true})}
      />,
    )
    fireEvent.click(screen.getByRole('button', {name: 'Open settings'}))
    fireEvent.click(screen.getByRole('button', {name: 'Open organisation settings'}))
    expect(openSettings).toHaveBeenCalledWith('accounts')
    expect(openSettings).toHaveBeenCalledWith('orgs')
    useUIStore.setState({openSettings: original})
  })

  it('a shared NOT_CONFIGURED failure offers only the organisation section', () => {
    const openSettings = vi.fn()
    const original = useUIStore.getState().openSettings
    useUIStore.setState({openSettings})
    render(
      <MessageBubble
        message={{
          ...msg({content: 'invalid shared config'}),
          finishReason: 'error',
          errorCode: 'PROVIDER_NOT_CONFIGURED',
          errorShared: true,
        }}
      />,
    )
    expect(screen.queryByRole('button', {name: 'Open settings'})).toBeNull()
    fireEvent.click(screen.getByRole('button', {name: 'Open organisation settings'}))
    expect(openSettings).toHaveBeenCalledWith('orgs')
    useUIStore.setState({openSettings: original})
  })

  it('re-renders when only the served model arrives (memo comparator)', () => {
    const base = msg({content: 'hi', model: 'dep'})
    const {rerender} = render(<MessageBubble message={base} />)
    expect(screen.queryByTitle(/Answered by/)).toBeNull()
    rerender(<MessageBubble message={{...base, servedModel: 'gpt-4o'}} />)
    expect(screen.getByTitle('Answered by gpt-4o')).toBeInTheDocument()
    rerender(<MessageBubble message={{...base, model: 'dep-2', servedModel: 'gpt-4o'}} />)
    expect(screen.getByText('· dep-2')).toBeInTheDocument()
  })
})

// An Azure AI Foundry reply is sent as "m/<id>": the bubble names the model
// by the label it was sent with — the history keeps naming a model that is
// renamed or removed later.
describe('MessageBubble — model labels', () => {
  const azureMsg = (over: Partial<ChatMessage> = {}) =>
    msg({content: 'the docs', provider: 'azure-foundry', model: 'm/docs000000', ...over})

  it('names the model by its label, never by its registry id', () => {
    render(<MessageBubble message={azureMsg({modelLabel: 'Docs writer', servedModel: 'gpt-4.1-2025-04-14'})} />)
    expect(screen.getByText('· Docs writer')).toBeInTheDocument()
    expect(screen.queryByText(/m\/docs000000/)).toBeNull()
    // The served model still shows next to it.
    expect(screen.getByTitle('Answered by gpt-4.1-2025-04-14')).toBeInTheDocument()
  })

  it("takes the listing's name for a message without a label, but the message's own label wins", () => {
    const {rerender} = render(<MessageBubble message={azureMsg()} modelLabel="Docs writer" />)
    expect(screen.getByText('· Docs writer')).toBeInTheDocument()
    rerender(<MessageBubble message={azureMsg({modelLabel: 'Docs (at the time)'})} modelLabel="Docs writer" />)
    expect(screen.getByText('· Docs (at the time)')).toBeInTheDocument()
  })

  it('reads "removed model" for a registry id nothing names any more', () => {
    render(<MessageBubble message={azureMsg()} />)
    expect(screen.getByText('· removed model')).toBeInTheDocument()
  })

  // A label that is only the id is no label: a turn sent before the listing
  // loaded stored its raw id there.
  it('takes a label that is only the id for none', () => {
    const {rerender} = render(
      <MessageBubble message={azureMsg({modelLabel: 'm/docs000000'})} modelLabel="Docs writer" />,
    )
    expect(screen.getByText('· Docs writer')).toBeInTheDocument()
    rerender(<MessageBubble message={azureMsg({modelLabel: 'm/docs000000'})} />)
    expect(screen.getByText('· removed model')).toBeInTheDocument()
    expect(screen.queryByText(/m\/docs000000/)).toBeNull()
  })

  // While nothing can be sent, Retry and Regenerate stay reachable and
  // say why they do nothing.
  it('keeps Retry and Regenerate reachable but unavailable while nothing can be sent, with the reason', () => {
    const onRegenerate = vi.fn()
    const onRetry = vi.fn()
    const reason = 'No Azure AI Foundry models yet.'
    const {rerender} = render(
      <MessageBubble message={azureMsg()} isLastAssistant onRegenerate={onRegenerate} resendDisabledReason={reason} />,
    )
    const regenerate = screen.getByRole('button', {name: 'Regenerate'})
    expect(regenerate).toHaveAttribute('aria-disabled', 'true')
    expect(regenerate).toHaveAccessibleDescription(reason)
    fireEvent.click(regenerate)
    expect(onRegenerate).not.toHaveBeenCalled()

    rerender(
      <MessageBubble
        message={azureMsg({id: 'm2', finishReason: 'error', content: 'failed'})}
        isLastAssistant
        onRetry={onRetry}
        resendDisabledReason={reason}
      />,
    )
    fireEvent.click(screen.getByRole('button', {name: 'Retry'}))
    expect(onRetry).not.toHaveBeenCalled()
    expect(screen.getByRole('button', {name: 'Retry'})).toHaveAccessibleDescription(reason)

    // The reason going away re-renders (memo comparator), and Retry works.
    rerender(
      <MessageBubble
        message={azureMsg({id: 'm2', finishReason: 'error', content: 'failed'})}
        isLastAssistant
        onRetry={onRetry}
      />,
    )
    fireEvent.click(screen.getByRole('button', {name: 'Retry'}))
    expect(onRetry).toHaveBeenCalledTimes(1)
  })

  it('does not repeat a served model that is the label itself', () => {
    render(<MessageBubble message={azureMsg({modelLabel: 'gpt-4o', servedModel: 'gpt-4o'})} />)
    expect(screen.getAllByText('· gpt-4o')).toHaveLength(1)
  })

  it('re-renders when only a label arrives (memo comparator)', () => {
    const base = azureMsg()
    const {rerender} = render(<MessageBubble message={base} />)
    expect(screen.getByText('· removed model')).toBeInTheDocument()
    rerender(<MessageBubble message={base} modelLabel="Docs writer" />)
    expect(screen.getByText('· Docs writer')).toBeInTheDocument()
    rerender(<MessageBubble message={{...base, modelLabel: 'Renamed'}} modelLabel="Docs writer" />)
    expect(screen.getByText('· Renamed')).toBeInTheDocument()
  })

  it('offers "Choose another model" (not Settings) on a MODEL_NOT_AVAILABLE bubble, opening the picker', () => {
    const before = useChatStore.getState().modelPickerRequest
    render(
      <MessageBubble
        message={azureMsg({
          content: '**This model is no longer available** — gone',
          finishReason: 'error',
          errorCode: 'MODEL_NOT_AVAILABLE',
        })}
      />,
    )
    expect(screen.queryByRole('button', {name: 'Open settings'})).toBeNull()
    fireEvent.click(screen.getByRole('button', {name: 'Choose another model'}))
    expect(useChatStore.getState().modelPickerRequest).toBe(before + 1)
  })
})
