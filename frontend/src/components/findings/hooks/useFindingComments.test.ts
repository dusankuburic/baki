import {describe, it, expect, vi, beforeEach} from 'vitest'
import {renderHook, act} from '@testing-library/react'

const addComment = vi.fn()
const listComments = vi.fn()

vi.mock('@/api', () => ({
  analysisApi: {
    addComment: (...args: unknown[]) => addComment(...args),
    listComments: (...args: unknown[]) => listComments(...args),
  },
}))

const toastError = vi.fn()
vi.mock('@/components/shared', () => ({
  useToast: () => ({error: toastError, success: vi.fn()}),
}))

vi.mock('react-i18next', () => ({
  useTranslation: () => ({t: (k: string) => k}),
}))

vi.mock('@/stores/analysisStore', () => ({
  findingKey: () => 'finding-key',
}))

import {useFindingComments} from './useFindingComments'
import type {Finding, FlowDocument} from '@/types'

const finding = {ruleId: 'unhandled-error'} as unknown as Finding
const doc = {id: 'flow-1'} as unknown as FlowDocument

describe('useFindingComments', () => {
  beforeEach(() => {
    addComment.mockReset()
    listComments.mockReset()
    toastError.mockReset()
  })

  it('restores what was typed when the POST fails', async () => {
    addComment.mockRejectedValue(new Error('500 internal server error'))
    const {result} = renderHook(() => useFindingComments(finding, doc))

    const typed = 'Accepted: compensating control documented in RISK-4471.'
    act(() => result.current.setCommentBody(typed))

    await act(async () => {
      await result.current.handleSubmitComment()
    })

    expect(toastError).toHaveBeenCalled()
    expect(result.current.commentBody).toBe(typed)
  })

  it('clears the composer when the POST succeeds', async () => {
    addComment.mockResolvedValue({id: 'c1', body: 'ok'})
    const {result} = renderHook(() => useFindingComments(finding, doc))

    act(() => result.current.setCommentBody('looks fine'))
    await act(async () => {
      await result.current.handleSubmitComment()
    })

    expect(result.current.commentBody).toBe('')
  })

  it('does not clobber newer text typed during the request', async () => {
    let reject: (e: Error) => void = () => {}
    addComment.mockImplementation(() => new Promise((_, r) => (reject = r)))
    const {result} = renderHook(() => useFindingComments(finding, doc))

    act(() => result.current.setCommentBody('first attempt'))
    let submitted: Promise<void>
    act(() => {
      submitted = result.current.handleSubmitComment()
    })
    act(() => result.current.setCommentBody('a newer draft'))
    await act(async () => {
      reject(new Error('boom'))
      await submitted
    })

    expect(result.current.commentBody).toBe('a newer draft')
  })
})
