import {describe, it, expect, vi} from 'vitest'
import {render, screen, fireEvent} from '@testing-library/react'
import FileList from './FileList'
import type {FlowFileInfo} from '@/types'

const files: FlowFileInfo[] = [
  {path: '/f/Main.txt', name: 'Main.txt', size: 500},
  {path: '/f/Zeta.txt', name: 'Zeta.txt', size: 2000},
  {path: '/f/Alpha.txt', name: 'Alpha.txt', size: 100},
  {path: '/f/Beta.txt', name: 'Beta.txt', size: 50},
  {path: '/f/Gamma.txt', name: 'Gamma.txt', size: 10},
]

describe('FileList', () => {
  it('sorts by name by default', () => {
    render(<FileList files={files} selectedFilePath={null} folderName="f" onSelectFile={vi.fn()} />)
    const names = screen.getAllByRole('option').map(el => el.querySelector('span')?.textContent)
    expect(names).toEqual(['Alpha', 'Beta', 'Gamma', 'Main', 'Zeta'])
  })

  it('toggles to size sort (descending) and back', () => {
    render(<FileList files={files} selectedFilePath={null} folderName="f" onSelectFile={vi.fn()} />)
    fireEvent.click(screen.getByTitle(/sort by size/i))
    let names = screen.getAllByRole('option').map(el => el.querySelector('span')?.textContent)
    expect(names).toEqual(['Zeta', 'Main', 'Alpha', 'Beta', 'Gamma'])

    fireEvent.click(screen.getByTitle(/sort by name/i))
    names = screen.getAllByRole('option').map(el => el.querySelector('span')?.textContent)
    expect(names).toEqual(['Alpha', 'Beta', 'Gamma', 'Main', 'Zeta'])
  })

  it('filters files by name', () => {
    render(<FileList files={files} selectedFilePath={null} folderName="f" onSelectFile={vi.fn()} />)
    fireEvent.change(screen.getByPlaceholderText('Filter files…'), {target: {value: 'ta'}})
    const names = screen.getAllByRole('option').map(el => el.querySelector('span')?.textContent)
    expect(names).toEqual(['Beta', 'Zeta'])
  })

  it('shows an empty state when the filter matches nothing', () => {
    render(<FileList files={files} selectedFilePath={null} folderName="f" onSelectFile={vi.fn()} />)
    fireEvent.change(screen.getByPlaceholderText('Filter files…'), {target: {value: 'zzz'}})
    expect(screen.getByText(/No files match/)).toBeInTheDocument()
  })

  it('navigates with arrow keys and opens the focused file on Enter', () => {
    const onSelectFile = vi.fn()
    render(<FileList files={files} selectedFilePath={null} folderName="f" onSelectFile={onSelectFile} />)
    const list = screen.getByRole('listbox')
    // The list starts focused on the first row (index 0: Alpha); two
    // ArrowDowns move to index 2 (sorted by name: Alpha, Beta, Gamma, ...).
    fireEvent.keyDown(list, {key: 'ArrowDown'})
    fireEvent.keyDown(list, {key: 'ArrowDown'})
    fireEvent.keyDown(list, {key: 'Enter'})
    expect(onSelectFile).toHaveBeenCalledWith('/f/Gamma.txt')
  })

  it('does not show the filter/sort toolbar for small file counts', () => {
    render(<FileList files={files.slice(0, 3)} selectedFilePath={null} folderName="f" onSelectFile={vi.fn()} />)
    expect(screen.queryByPlaceholderText('Filter files…')).not.toBeInTheDocument()
  })

  it('opens a context menu with reveal/reload wired through', () => {
    const onRevealFile = vi.fn()
    const onReloadFile = vi.fn()
    render(
      <FileList
        files={files}
        selectedFilePath={null}
        folderName="f"
        onSelectFile={vi.fn()}
        onRevealFile={onRevealFile}
        onReloadFile={onReloadFile}
      />,
    )
    fireEvent.contextMenu(screen.getAllByRole('option')[0])
    fireEvent.click(screen.getByText('Reveal in file manager'))
    expect(onRevealFile).toHaveBeenCalledWith('/f/Alpha.txt')

    fireEvent.contextMenu(screen.getAllByRole('option')[0])
    fireEvent.click(screen.getByText('Reload from disk'))
    expect(onReloadFile).toHaveBeenCalledWith('/f/Alpha.txt')
  })
})
