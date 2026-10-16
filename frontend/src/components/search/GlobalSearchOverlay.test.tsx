import {describe, it, expect, vi, beforeEach, afterEach} from 'vitest'
import {render, screen, fireEvent, act} from '@testing-library/react'
import GlobalSearchOverlay from './GlobalSearchOverlay'
import {flowApi, ApiError} from '@/api'

describe('GlobalSearchOverlay all-flows search', () => {
  beforeEach(() => {
    vi.useFakeTimers()
  })
  afterEach(() => {
    vi.useRealTimers()
    vi.restoreAllMocks()
  })

  const typeQuery = async (text: string) => {
    fireEvent.change(screen.getByRole('combobox'), {target: {value: text}})
    await act(async () => {
      await vi.advanceTimersByTimeAsync(300)
    })
  }

  it('does not send a term without three letters or digits in a row and says why', async () => {
    const search = vi.spyOn(flowApi, 'searchLibrary').mockResolvedValue({
      query: {text: '', fuzzy: true, maxResults: 50},
      results: [],
      totalCount: 0,
      durationMs: 0,
    })
    render(<GlobalSearchOverlay isOpen onClose={vi.fn()} />)
    fireEvent.click(screen.getByRole('button', {name: 'All Flows'}))

    // Punctuation and spaces split words for the server's index, so "---" or "a b" is as short as "ab".
    for (const text of ['a', 'ab', ' ab ', '日本', '---', '%%%', 'a b', 'a-b-c']) {
      await typeQuery(text)
      expect(
        screen.getByText('Type at least 3 letters or digits in a row to search across all your flows'),
      ).toBeInTheDocument()
    }
    expect(search).not.toHaveBeenCalled()

    // A vowel sign inside a Devanagari or Thai word does not split it, for the server's index either.
    for (const text of ['abc', '日本語', 'x-abc', 'नाम', 'สวัสดี']) {
      await typeQuery(text)
      expect(search).toHaveBeenLastCalledWith(expect.objectContaining({text}))
    }
    expect(search).toHaveBeenCalledTimes(5)
  })

  // The server decides with its own Unicode tables and its database's (a letter newer than them, or any non-ASCII
  // letter under a C ctype, is no word character there): its refusal shows the same hint, not "No matches".
  it('shows the minimum hint when the server refuses a term the client let through', async () => {
    vi.spyOn(flowApi, 'searchLibrary').mockRejectedValue(
      new ApiError('a library search needs at least 3 letters or digits in a row', 400, 'QUERY_TOO_SHORT', null, {
        minLength: 3,
      }),
    )
    render(<GlobalSearchOverlay isOpen onClose={vi.fn()} />)
    fireEvent.click(screen.getByRole('button', {name: 'All Flows'}))
    await typeQuery('\u{31350}\u{31351}\u{31352}')
    expect(screen.queryByText(/needs at least/)).not.toBeInTheDocument()
    expect(screen.queryByText(/No matches found/)).not.toBeInTheDocument()
    expect(
      screen.getByText('Type at least 3 letters or digits in a row to search across all your flows'),
    ).toBeInTheDocument()
  })

  it('keeps the minimum hint when a refused term has surrounding whitespace or filters toggle', async () => {
    vi.spyOn(flowApi, 'searchLibrary').mockRejectedValue(
      new ApiError('a library search needs at least 3 letters or digits in a row', 400, 'QUERY_TOO_SHORT', null, {
        minLength: 3,
      }),
    )
    render(<GlobalSearchOverlay isOpen onClose={vi.fn()} />)
    fireEvent.click(screen.getByRole('button', {name: 'All Flows'}))

    // Padded term: leading or trailing space
    await typeQuery('\u{31350}\u{31351}\u{31352} ')
    expect(screen.queryByText(/No matches found/)).not.toBeInTheDocument()
    expect(
      screen.getByText('Type at least 3 letters or digits in a row to search across all your flows'),
    ).toBeInTheDocument()

    // Type '日本語' then add a trailing space: hint must stay
    await typeQuery('日本語')
    expect(
      screen.getByText('Type at least 3 letters or digits in a row to search across all your flows'),
    ).toBeInTheDocument()
    await typeQuery('日本語 ')
    expect(screen.queryByText(/No matches found/)).not.toBeInTheDocument()
    expect(
      screen.getByText('Type at least 3 letters or digits in a row to search across all your flows'),
    ).toBeInTheDocument()

    // Toggle a type filter after a refusal
    const filterBtn = screen.getByRole('button', {name: /Action/i})
    fireEvent.click(filterBtn)
    await act(async () => {
      await vi.advanceTimersByTimeAsync(300)
    })
    expect(screen.queryByText(/No matches found/)).not.toBeInTheDocument()
    expect(
      screen.getByText('Type at least 3 letters or digits in a row to search across all your flows'),
    ).toBeInTheDocument()
  })
})
