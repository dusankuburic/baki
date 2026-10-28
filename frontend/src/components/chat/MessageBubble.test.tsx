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

  it('renders a normal external link (no block scheme) as an anchor once the user asks to see it', () => {
    render(<MessageBubble message={msg({content: 'docs at [here](https://example.com).'})} />)
    expect(screen.queryByRole('link')).toBeNull()
    fireEvent.click(screen.getByRole('button', {name: /here/}))
    const link = screen.getByRole('link', {name: /example\.com/})
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

describe('MessageBubble — model labels', () => {
  const azureMsg = (over: Partial<ChatMessage> = {}) =>
    msg({content: 'the docs', provider: 'azure-foundry', model: 'm/docs000000', ...over})

  it('names the model by its label, never by its registry id', () => {
    render(<MessageBubble message={azureMsg({modelLabel: 'Docs writer', servedModel: 'gpt-4.1-2025-04-14'})} />)
    expect(screen.getByText('· Docs writer')).toBeInTheDocument()
    expect(screen.queryByText(/m\/docs000000/)).toBeNull()
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

  it('takes a label that is only the id for none', () => {
    const {rerender} = render(
      <MessageBubble message={azureMsg({modelLabel: 'm/docs000000'})} modelLabel="Docs writer" />,
    )
    expect(screen.getByText('· Docs writer')).toBeInTheDocument()
    rerender(<MessageBubble message={azureMsg({modelLabel: 'm/docs000000'})} />)
    expect(screen.getByText('· removed model')).toBeInTheDocument()
    expect(screen.queryByText(/m\/docs000000/)).toBeNull()
  })

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

describe('MessageBubble — persisted approvals', () => {
  it('renders non-fix approvals and a fix proposal of another kind generically, and cancels pending ones on an interrupted message', () => {
    render(
      <MessageBubble
        message={msg({
          content: 'done',
          finishReason: 'interrupted',
          fixProposals: [
            {
              proposalId: 'p1',
              ruleId: 'unhandled-error',
              fixType: 'wrap-error-handler',
              blockLabel: 'Call API',
              line: 3,
              summary: 'wrap',
              status: 'applied',
            },
            {
              proposalId: 'p2',
              tool: 'rename_variable',
              previewKind: 'something-new',
              ruleId: '',
              fixType: '',
              blockLabel: 'Rename x',
              line: 0,
              summary: 'x → total',
              status: 'pending',
            },
          ],
          approvals: [
            {
              proposalId: 'a1',
              tool: 'delete_block',
              previewKind: 'something-else',
              status: 'pending',
              items: [{id: 'b1', label: 'Block 1', summary: '', status: 'pending'}],
            },
          ],
        })}
      />,
    )
    expect(screen.getByTestId('fix-outcome')).toHaveTextContent('Fix applied and verified')
    const generic = screen.getAllByTestId('approval-outcome')
    expect(generic).toHaveLength(2)
    expect(generic[0]).toHaveTextContent('rename_variable')
    expect(generic[0]).toHaveTextContent('Rename x')
    expect(generic[0]).toHaveTextContent('Cancelled — nothing was changed.')
    expect(generic[1]).toHaveTextContent('delete_block')
    expect(generic[1]).toHaveTextContent('Block 1')
    expect(generic[1]).toHaveTextContent('Cancelled — nothing was changed.')
  })
})

// The omitted-history chip (R-streaming-7): a turn that lost earlier history says so, with the Compact action.
describe('MessageBubble — omitted history chip', () => {
  it('shows how many earlier turns were omitted, with a Compact action', () => {
    const onCompact = vi.fn()
    render(<MessageBubble message={msg({content: 'answer', truncated: 4})} onCompact={onCompact} />)
    expect(screen.getByText(/Earlier turns omitted \(4\)/)).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', {name: 'Compact'}))
    expect(onCompact).toHaveBeenCalledTimes(1)
  })

  it('renders the chip without the action when compacting is not offered', () => {
    render(<MessageBubble message={msg({content: 'answer', truncated: 2})} />)
    expect(screen.getByText(/Earlier turns omitted \(2\)/)).toBeInTheDocument()
    expect(screen.queryByRole('button', {name: 'Compact'})).toBeNull()
  })

  it('renders no chip for a message that lost nothing', () => {
    render(<MessageBubble message={msg({content: 'answer'})} />)
    expect(screen.queryByText(/Earlier turns omitted/)).toBeNull()
  })
})

// External links (R-security-2): a URL the model emits is flow- or document-derived text as far as the user can
// tell, so it never renders as a navigable link until the user asks for it; block: and finding: links still work.
describe('MessageBubble — external links', () => {
  it('does not render a navigable link until the user confirms it', () => {
    render(<MessageBubble message={msg({content: 'See [the docs](https://example.com/page) for more.'})} />)
    expect(screen.queryByRole('link')).toBeNull()
    fireEvent.click(screen.getByRole('button', {name: /the docs/}))
    const link = screen.getByRole('link', {name: /example\.com\/page/})
    expect(link).toHaveAttribute('href', 'https://example.com/page')
    expect(link).toHaveAttribute('target', '_blank')
    expect(link).toHaveAttribute('rel', expect.stringContaining('noopener'))
  })
})

describe('MessageBubble — coded errors (R-provider-16)', () => {
  const errorMsg = (errorCode: string) =>
    msg({content: '*Error: x*', finishReason: 'error', errorCode} as Partial<ChatMessage>)

  it('a context overflow offers Compact and a hint, and keeps Retry', () => {
    const onCompact = vi.fn()
    render(<MessageBubble message={errorMsg('CONTEXT_LIMIT')} onRetry={vi.fn()} onCompact={onCompact} />)
    expect(screen.getByRole('button', {name: /retry/i})).toBeInTheDocument()
    expect(screen.getByText(/narrow the question/i)).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', {name: 'Compact'}))
    expect(onCompact).toHaveBeenCalledOnce()
  })

  it('a context overflow on a read-only flow keeps Retry, and Compact waits for a running reply', () => {
    const {unmount} = render(<MessageBubble message={errorMsg('CONTEXT_LIMIT')} onRetry={vi.fn()} />)
    expect(screen.getByRole('button', {name: /retry/i})).toBeInTheDocument()
    expect(screen.queryByRole('button', {name: 'Compact'})).toBeNull()
    unmount()
    render(
      <MessageBubble
        message={errorMsg('CONTEXT_LIMIT')}
        onRetry={vi.fn()}
        onCompact={vi.fn()}
        resendDisabledReason="A reply is still being written"
      />,
    )
    expect(screen.getByRole('button', {name: 'Compact'})).toBeDisabled()
  })

  it('a step-limit failure shows its hint and keeps Retry', () => {
    render(<MessageBubble message={errorMsg('TOOL_LOOP_LIMIT')} onRetry={vi.fn()} />)
    expect(screen.getByText(/narrower question/i)).toBeInTheDocument()
    expect(screen.getByRole('button', {name: /retry/i})).toBeInTheDocument()
  })

  it('a timeout keeps the plain Retry', () => {
    render(<MessageBubble message={errorMsg('STREAM_TIMEOUT')} onRetry={vi.fn()} onCompact={vi.fn()} />)
    expect(screen.getByRole('button', {name: /retry/i})).toBeInTheDocument()
    expect(screen.queryByRole('button', {name: 'Compact'})).toBeNull()
  })
})

// The knowledge notice (R-knowledge-1): the org's knowledge base was not consulted for this answer, and why.
describe('MessageBubble — knowledge notice', () => {
  it.each(['identity_mismatch', 'no_embedder', 'deadline'] as const)('renders the %s notice', status => {
    render(<MessageBubble message={msg({content: 'answer', knowledge: status})} />)
    // The notice without its icon's drawing, which belongs to the icon library.
    const notice = screen.getByTestId('knowledge-notice').cloneNode(true) as HTMLElement
    notice.querySelector('svg')?.replaceChildren()
    expect(notice).toMatchSnapshot()
  })

  it('re-renders when a message gains its knowledge status (the memo compares it)', () => {
    const at = '2026-10-27T10:00:00Z'
    const {rerender} = render(<MessageBubble message={msg({content: 'answer', timestamp: at})} />)
    expect(screen.queryByTestId('knowledge-notice')).toBeNull()
    rerender(<MessageBubble message={msg({content: 'answer', timestamp: at, knowledge: 'deadline'})} />)
    expect(screen.getByTestId('knowledge-notice')).toHaveTextContent(/did not answer in time/)
  })

  it('renders no notice when the knowledge base was consulted', () => {
    render(<MessageBubble message={msg({content: 'answer'})} />)
    expect(screen.queryByTestId('knowledge-notice')).toBeNull()
  })
})
