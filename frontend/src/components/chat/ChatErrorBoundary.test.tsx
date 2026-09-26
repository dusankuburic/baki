import {fireEvent, render, screen} from '@testing-library/react'
import {afterEach, beforeEach, describe, expect, it, vi} from 'vitest'
import {reloadForChunkError, reloadPage} from '@/lib/chunkLoad'
import ChatErrorBoundary from './ChatErrorBoundary'

vi.mock('@/lib/chunkLoad', async importOriginal => ({
  ...(await importOriginal<typeof import('@/lib/chunkLoad')>()),
  reloadForChunkError: vi.fn(() => false),
  reloadPage: vi.fn(),
}))

let failure: Error | null = null

function Thread() {
  if (failure) throw failure
  return <p>thread</p>
}

describe('ChatErrorBoundary', () => {
  beforeEach(() => {
    vi.spyOn(console, 'error').mockImplementation(() => {})
  })
  afterEach(() => {
    failure = null
    vi.clearAllMocks()
  })

  it('offers a page reload when a lazy chunk failed to download', () => {
    failure = new TypeError(
      'Failed to fetch dynamically imported module: http://localhost:8080/assets/ChatHelpPopover.js',
    )
    render(
      <ChatErrorBoundary>
        <Thread />
      </ChatErrorBoundary>,
    )
    expect(reloadForChunkError).toHaveBeenCalledOnce()
    expect(screen.getByText("This part of the app didn't load")).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', {name: 'Reload page'}))
    expect(reloadPage).toHaveBeenCalledOnce()
  })

  it('retries in place for other errors', () => {
    failure = new Error('bad message shape')
    render(
      <ChatErrorBoundary>
        <Thread />
      </ChatErrorBoundary>,
    )
    expect(screen.getByText('Chat panel error')).toBeInTheDocument()
    failure = null
    fireEvent.click(screen.getByRole('button', {name: 'Try again'}))
    expect(screen.getByText('thread')).toBeInTheDocument()
    expect(reloadPage).not.toHaveBeenCalled()
  })
})
