import {fireEvent, render, screen} from '@testing-library/react'
import {afterEach, beforeEach, describe, expect, it, vi} from 'vitest'
import {systemApi} from '@/api'
import {reloadForChunkError, reloadPage} from '@/lib/chunkLoad'
import ErrorBoundary from './ErrorBoundary'

vi.mock('@/lib/chunkLoad', async importOriginal => ({
  ...(await importOriginal<typeof import('@/lib/chunkLoad')>()),
  reloadForChunkError: vi.fn(() => false),
  reloadPage: vi.fn(),
}))
vi.mock('@/api', () => ({systemApi: {logError: vi.fn(() => Promise.resolve())}}))

const chunkError = () =>
  new TypeError('Failed to fetch dynamically imported module: http://localhost:8080/assets/AITab-CWH9-ePf.js')

function Throws({error}: {error: Error}): null {
  throw error
}

describe('ErrorBoundary', () => {
  beforeEach(() => {
    vi.spyOn(console, 'error').mockImplementation(() => {})
  })
  afterEach(() => vi.clearAllMocks())

  it('offers a page reload, not a retry, when a lazy chunk failed to download', () => {
    render(
      <ErrorBoundary fallbackMessage="AI assistant error">
        <Throws error={chunkError()} />
      </ErrorBoundary>,
    )
    expect(reloadForChunkError).toHaveBeenCalledOnce()
    expect(screen.getByText("This part of the app didn't load")).toBeInTheDocument()
    expect(screen.queryByRole('button', {name: 'Try again'})).not.toBeInTheDocument()

    fireEvent.click(screen.getByRole('button', {name: 'Reload page'}))
    expect(reloadPage).toHaveBeenCalledOnce()
  })

  it('reloads without reporting when the automatic reload is allowed', () => {
    vi.mocked(reloadForChunkError).mockReturnValueOnce(true)
    render(
      <ErrorBoundary>
        <Throws error={chunkError()} />
      </ErrorBoundary>,
    )
    expect(systemApi.logError).not.toHaveBeenCalled()
  })

  it('keeps Try again for other render errors', () => {
    render(
      <ErrorBoundary fallbackMessage="AI assistant error">
        <Throws error={new Error('boom')} />
      </ErrorBoundary>,
    )
    expect(reloadForChunkError).not.toHaveBeenCalled()
    expect(screen.getByText('AI assistant error')).toBeInTheDocument()
    expect(screen.getByText('boom')).toBeInTheDocument()
    expect(screen.getByRole('button', {name: 'Try again'})).toBeInTheDocument()
    expect(systemApi.logError).toHaveBeenCalledOnce()
  })
})
