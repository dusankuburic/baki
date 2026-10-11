import {describe, it, expect, vi} from 'vitest'
import {render, screen, fireEvent} from '@testing-library/react'
import LibraryList from './LibraryList'
import type {LibraryFlow} from '@/api/library'

function flow(id: string, name: string): LibraryFlow {
  return {
    id,
    name,
    ownerId: 'me',
    ownerDisplayName: 'me@example.com',
    organizationId: '',
    blockCount: 116,
    subflowCount: 13,
    updatedAt: '2026-01-01T00:00:00Z',
    version: 1,
    isSharedWithMe: false,
    canEdit: true,
    canDelete: true,
    canShare: true,
  }
}

describe('LibraryList', () => {
  it('names each row by its content, selects on click and opens on Enter once selected', () => {
    const onSelect = vi.fn()
    const onOpen = vi.fn()
    render(<LibraryList items={[flow('a', 'padframework')]} selectedId="a" onSelect={onSelect} onOpen={onOpen} />)

    const row = screen.getByRole('button', {name: /padframework/})
    expect(row).not.toHaveAttribute('aria-label')

    fireEvent.click(row)
    expect(onSelect).toHaveBeenCalledWith(expect.objectContaining({id: 'a'}))
    fireEvent.keyDown(row, {key: 'Enter'})
    expect(onOpen).toHaveBeenCalledWith(expect.objectContaining({id: 'a'}))
  })

  it('shows a spinner on the row being opened', () => {
    render(
      <LibraryList
        items={[flow('a', 'padframework')]}
        selectedId={null}
        onSelect={() => {}}
        onOpen={() => {}}
        openingId="a"
      />,
    )
    expect(screen.getByRole('status')).toBeInTheDocument()
  })

  it('tags the owner and subflows cells, heading included, so a narrow pane drops each column whole', () => {
    const {container} = render(
      <LibraryList items={[flow('a', 'A'), flow('b', 'B')]} selectedId={null} onSelect={() => {}} onOpen={() => {}} />,
    )
    expect(container.querySelectorAll('.library-row')).toHaveLength(3)
    expect(container.querySelectorAll('.library-col-owner')).toHaveLength(3)
    expect(container.querySelectorAll('.library-col-subflows')).toHaveLength(3)
  })
})
