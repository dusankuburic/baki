import {describe, it, expect, vi, beforeEach} from 'vitest'
import {render, screen, fireEvent, within} from '@testing-library/react'
import {DataTable} from './DataTable'

const downloadBlob = vi.fn()
vi.mock('@/lib/csv', async importOriginal => {
  const actual = await importOriginal<typeof import('@/lib/csv')>()
  return {...actual, downloadBlob: (...a: unknown[]) => downloadBlob(...a)}
})

const columns = [
  {key: 'rule', header: 'Rule'},
  {key: 'count', header: 'Findings', align: 'right' as const},
]
const rows = [
  {rule: 'missing-timeout', count: 12},
  {rule: '=cmd|calc', count: 3},
]

beforeEach(() => downloadBlob.mockClear())

describe('DataTable', () => {
  it('renders a captioned table with right-aligned numbers', () => {
    render(<DataTable caption="Findings by rule" columns={columns} rows={rows} />)
    const table = screen.getByRole('table', {name: 'Findings by rule'})
    expect(
      within(table)
        .getAllByRole('columnheader')
        .map(h => h.textContent),
    ).toEqual(['Rule', 'Findings'])
    expect(within(table).getByText('12')).toHaveClass('text-right')
  })

  it('downloads a CSV with formula-injection cells escaped', () => {
    render(<DataTable caption="Findings by rule" columns={columns} rows={rows} filename="rules.csv" />)
    fireEvent.click(screen.getByRole('button', {name: 'Download CSV'}))
    expect(downloadBlob).toHaveBeenCalledTimes(1)
    const [content, mime, filename] = downloadBlob.mock.calls[0] as [string, string, string]
    expect(mime).toContain('text/csv')
    expect(filename).toBe('rules.csv')
    const lines = content.split('\n')
    expect(lines[0]).toBe('Rule,Findings')
    expect(lines[1]).toBe('missing-timeout,12')
    // The leading "=" is neutralised so a spreadsheet never runs it.
    expect(lines[2]).not.toMatch(/^=/)
    expect(lines[2]).toContain('cmd|calc')
  })

  it('tells the reader when rows are held back on screen', () => {
    const many = Array.from({length: 5}, (_, i) => ({rule: `r${i}`, count: i}))
    render(<DataTable caption="c" columns={columns} rows={many} maxRows={2} />)
    expect(screen.getAllByRole('row')).toHaveLength(3)
    expect(screen.getByText(/Showing 2 of 5 rows/)).toBeInTheDocument()
  })
})
