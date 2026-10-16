import {describe, it, expect, vi} from 'vitest'
import {render, screen, fireEvent} from '@testing-library/react'
import Pager from './Pager'

describe('Pager', () => {
  it('announces the visible range against the total and stops at the last page', () => {
    const onPageChange = vi.fn()
    const {rerender} = render(
      <Pager page={0} pageSize={10} count={10} total={17} onPageChange={onPageChange} label="Users pagination" />,
    )
    const nav = screen.getByRole('navigation', {name: 'Users pagination'})
    expect(nav).toHaveTextContent('Showing 1–10 of 17')
    expect(screen.getByRole('button', {name: 'Previous page'})).toBeDisabled()
    expect(screen.getByRole('button', {name: 'Next page'})).toBeEnabled()

    fireEvent.click(screen.getByRole('button', {name: 'Next page'}))
    expect(onPageChange).toHaveBeenCalledWith(1)

    rerender(<Pager page={1} pageSize={10} count={7} total={17} onPageChange={onPageChange} label="Users pagination" />)
    expect(nav).toHaveTextContent('Showing 11–17 of 17')
    expect(screen.getByRole('button', {name: 'Next page'})).toBeDisabled()
    expect(screen.getByRole('button', {name: 'Previous page'})).toBeEnabled()
    fireEvent.click(screen.getByRole('button', {name: 'Previous page'}))
    expect(onPageChange).toHaveBeenLastCalledWith(0)
  })

  it('falls back to hasMore when the server reports no total', () => {
    const {rerender} = render(
      <Pager page={1} pageSize={15} count={15} hasMore onPageChange={() => {}} label="Audit log pagination" />,
    )
    expect(screen.getByRole('navigation', {name: 'Audit log pagination'})).toHaveTextContent('Showing 16–30')
    expect(screen.getByRole('button', {name: 'Next page'})).toBeEnabled()

    rerender(
      <Pager page={1} pageSize={15} count={15} hasMore={false} onPageChange={() => {}} label="Audit log pagination" />,
    )
    expect(screen.getByRole('button', {name: 'Next page'})).toBeDisabled()
  })

  it('keeps Next disabled when told to, whatever the total says', () => {
    render(
      <Pager
        page={0}
        pageSize={10}
        count={10}
        total={17}
        nextDisabled
        onPageChange={() => {}}
        label="Users pagination"
      />,
    )
    expect(screen.getByRole('button', {name: 'Next page'})).toBeDisabled()
    expect(screen.getByRole('navigation', {name: 'Users pagination'})).toHaveTextContent('Showing 1–10 of 17')
  })

  it('offers the page sizes and reports a change', () => {
    const onPageSizeChange = vi.fn()
    render(
      <Pager
        page={0}
        pageSize={10}
        count={10}
        total={40}
        onPageChange={() => {}}
        pageSizeOptions={[10, 25, 50]}
        onPageSizeChange={onPageSizeChange}
        label="Users pagination"
      />,
    )
    const select = screen.getByRole('combobox', {name: 'Rows per page'})
    expect(select).toHaveValue('10')
    fireEvent.change(select, {target: {value: '25'}})
    expect(onPageSizeChange).toHaveBeenCalledWith(25)
  })

  it('says so when the page is empty, in a live region', () => {
    render(<Pager page={0} pageSize={10} count={0} total={0} onPageChange={() => {}} label="Users pagination" />)
    const status = screen.getByText('No rows')
    expect(status).toHaveAttribute('aria-live', 'polite')
    expect(screen.getByRole('button', {name: 'Next page'})).toBeDisabled()
  })
})
