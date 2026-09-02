import {describe, it, expect, beforeEach, vi} from 'vitest'
import {render, screen, fireEvent} from '@testing-library/react'
import MessageBubble from './MessageBubble'
import {useFlowStore} from '@/stores/flowStore'
import {useUIStore} from '@/stores/uiStore'
import {useAnalysisStore} from '@/stores/analysisStore'
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
