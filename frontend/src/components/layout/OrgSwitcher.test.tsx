import {describe, it, expect, beforeEach, vi} from 'vitest'
import {render, screen, fireEvent} from '@testing-library/react'

const isTauriMock = vi.fn(() => false)
vi.mock('@/platform/guards', () => ({
  isTauri: () => isTauriMock(),
  isWeb: () => !isTauriMock(),
}))

// Partial mock (importOriginal) rather than a hand-written factory: the factory
// form silently omits any export the module gains later, and vitest turns the
// missing binding into a THROW at the call site. That is exactly what happened
// here — orgStore.setActiveOrg calls clearRequestCache(), which the old factory
// did not provide, so every org switch threw mid-action. The switching tests
// still passed (activeOrgId is written before the throw) while the cross-store
// reset that follows it never ran, and the run itself exited non-zero on the
// unhandled errors. Only `request` needs stubbing; everything else is inert.
vi.mock('@/api/client', async importOriginal => {
  const actual = await importOriginal<typeof import('@/api/client')>()
  return {
    ...actual,
    request: vi.fn().mockResolvedValue([]),
  }
})

import OrgSwitcher from './OrgSwitcher'
import {useOrgStore, type Organisation} from '@/stores/orgStore'
import {useAuthStore} from '@/stores/authStore'
import {useFlowStore} from '@/stores/flowStore'
import {useAnalysisStore} from '@/stores/analysisStore'
import {useChatStore} from '@/stores/chatStore'
import type {AnalysisReport, ChatMessage, FlowDocument} from '@/types'

function makeOrg(id: string, name: string): Organisation {
  return {
    id,
    name,
    ownerId: 'u1',
    members: [],
    createdAt: '2024-01-01T00:00:00Z',
    updatedAt: '2024-01-01T00:00:00Z',
  }
}

function makeDoc(): FlowDocument {
  return {
    id: 'flow1',
    name: 'My Flow',
    subflows: [{id: 'sf1', name: 'Main', blocks: []}],
  } as unknown as FlowDocument
}

// Seeds the stores an org switch is contractually required to wipe, so the
// tests below can prove the wipe happened rather than assuming it.
function seedPreviousOrgState() {
  useFlowStore.setState({document: makeDoc()})
  useAnalysisStore.setState({reports: new Map([['flow1', {findings: []} as unknown as AnalysisReport]])})
  useChatStore.setState({
    threads: [
      {
        id: 't1',
        flowId: 'flow1',
        title: 'Prev org thread',
        createdAt: '2024-01-01T00:00:00Z',
        contextBlockId: null,
        selectedSourceFiles: [],
        tokensIn: 0,
        tokensOut: 0,
      },
    ],
    activeThreadId: 't1',
    conversations: new Map([['t1', [{id: 'm1', role: 'user', content: 'hi'} as ChatMessage]]]),
  })
}

// Asserts the full cross-store teardown documented in orgStore.setActiveOrg:
// analysis reports and chat threads are not org-scoped in frontend state, so an
// org switch must clear them outright or the new org sees the old one's data.
function expectPreviousOrgStateCleared() {
  expect(useFlowStore.getState().document).toBeNull()
  expect(useAnalysisStore.getState().reports.size).toBe(0)
  expect(useChatStore.getState().threads).toEqual([])
  expect(useChatStore.getState().activeThreadId).toBeNull()
  expect(useChatStore.getState().conversations.size).toBe(0)
}

const initialOrgState = useOrgStore.getState()
const initialFlowState = useFlowStore.getState()
const initialAnalysisState = useAnalysisStore.getState()
const initialChatState = useChatStore.getState()

beforeEach(() => {
  isTauriMock.mockReturnValue(false)
  useOrgStore.setState(initialOrgState, true)
  useFlowStore.setState(initialFlowState, true)
  useAnalysisStore.setState(initialAnalysisState, true)
  useChatStore.setState(initialChatState, true)
  useAuthStore.setState({isAuthenticated: true})
})

describe('OrgSwitcher', () => {
  it('defaults to Personal when no org is active', () => {
    render(<OrgSwitcher />)
    expect(screen.getByText('Personal')).toBeInTheDocument()
  })

  it('shows the active org name in the trigger', () => {
    useOrgStore.setState({
      organisations: [makeOrg('org-1', 'Acme Corp')],
      activeOrgId: 'org-1',
    })
    render(<OrgSwitcher />)
    expect(screen.getByText('Acme Corp')).toBeInTheDocument()
  })

  it('switches the active org when an item is selected', () => {
    useOrgStore.setState({
      organisations: [makeOrg('org-1', 'Acme Corp')],
      activeOrgId: null,
    })
    seedPreviousOrgState()
    render(<OrgSwitcher />)

    fireEvent.click(screen.getByTitle('Switch organization'))
    fireEvent.click(screen.getByText('Acme Corp'))

    expect(useOrgStore.getState().activeOrgId).toBe('org-1')
    expectPreviousOrgStateCleared()
  })

  it('switches back to Personal', () => {
    useOrgStore.setState({
      organisations: [makeOrg('org-1', 'Acme Corp')],
      activeOrgId: 'org-1',
    })
    seedPreviousOrgState()
    render(<OrgSwitcher />)

    fireEvent.click(screen.getByTitle('Switch organization'))
    fireEvent.click(screen.getByText('Personal'))

    expect(useOrgStore.getState().activeOrgId).toBeNull()
    expectPreviousOrgStateCleared()
  })

  it('does not tear down flow/analysis/chat state when the org is unchanged', () => {
    useOrgStore.setState({
      organisations: [makeOrg('org-1', 'Acme Corp')],
      activeOrgId: 'org-1',
    })
    seedPreviousOrgState()
    render(<OrgSwitcher />)

    fireEvent.click(screen.getByTitle('Switch organization'))
    fireEvent.click(screen.getByText('✓ Acme Corp'))

    expect(useFlowStore.getState().document).not.toBeNull()
    expect(useAnalysisStore.getState().reports.size).toBe(1)
    expect(useChatStore.getState().threads).toHaveLength(1)
  })

  it('renders nothing in Tauri (desktop) mode', () => {
    isTauriMock.mockReturnValue(true)
    const {container} = render(<OrgSwitcher />)
    expect(container.firstChild).toBeNull()
  })

  it('renders nothing when not authenticated', () => {
    useAuthStore.setState({isAuthenticated: false})
    const {container} = render(<OrgSwitcher />)
    expect(container.firstChild).toBeNull()
  })
})
