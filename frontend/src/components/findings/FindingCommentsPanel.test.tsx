import {describe, it, expect, vi, beforeEach} from 'vitest'
import {render, screen, waitFor} from '@testing-library/react'
import {ToastProvider} from '@/components/shared/Toast'
import {ConfirmProvider} from '@/components/shared/ConfirmDialog'
import {useAuthStore} from '@/stores/authStore'
import {useFlowStore} from '@/stores/flowStore'
import type {FindingComment, FlowDocument} from '@/types'

const libraryGet = vi.fn()

vi.mock('@/api', () => ({
  analysisApi: {deleteComment: vi.fn()},
  libraryApi: {get: (...a: unknown[]) => libraryGet(...a)},
}))

import FindingCommentsPanel from './FindingCommentsPanel'

const comment = (over: Partial<FindingComment>): FindingComment => ({
  id: 'c1',
  flowId: 'flow-1',
  findingKey: 'fp-1',
  authorId: 'u-author-1234',
  authorName: 'Ada',
  body: 'kept words',
  createdAt: '2026-10-01T10:00:00Z',
  ...over,
})

function renderPanel(comments: FindingComment[]) {
  return render(
    <ToastProvider>
      <ConfirmProvider>
        <FindingCommentsPanel
          comments={comments}
          commentLoading={false}
          commentBody=""
          onCommentBodyChange={() => {}}
          onSubmit={() => {}}
        />
      </ConfirmProvider>
    </ToastProvider>,
  )
}

beforeEach(() => {
  vi.clearAllMocks()
  libraryGet.mockResolvedValue({canShare: false})
  useFlowStore.setState({document: {id: 'flow-1'} as unknown as FlowDocument})
  useAuthStore.setState({user: {id: 'u-me', email: 'me@example.com', role: 'member'}})
})

describe('FindingCommentsPanel', () => {
  it('names the author of an erased account instead of showing nothing', async () => {
    // Account erasure keeps the words and blanks the author: an empty id and no name.
    renderPanel([comment({id: 'c-erased', authorId: '', authorName: undefined})])
    expect(screen.getByText('kept words')).toBeInTheDocument()
    expect(screen.getByText('A former member')).toBeInTheDocument()
    // Nobody owns it any more, so only a moderator could remove it.
    await waitFor(() => expect(libraryGet).toHaveBeenCalledWith('flow-1'))
    expect(screen.queryByRole('button', {name: 'Delete comment'})).toBeNull()
  })

  it('keeps the name, or the id prefix when a live author has no name', () => {
    renderPanel([comment({}), comment({id: 'c2', authorId: 'u-nameless-5678', authorName: undefined, body: 'b'})])
    expect(screen.getByText('Ada')).toBeInTheDocument()
    expect(screen.getByText('u-namele')).toBeInTheDocument()
    expect(screen.queryByText('A former member')).toBeNull()
  })
})
