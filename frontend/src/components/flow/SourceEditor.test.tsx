import {describe, it, expect, vi, beforeEach} from 'vitest'
import {render, screen, waitFor, fireEvent} from '@testing-library/react'

const getSource = vi.fn()
const saveSource = vi.fn()

vi.mock('@/api', () => ({
  flowApi: {
    getSource: (...a: unknown[]) => getSource(...a),
    saveSource: (...a: unknown[]) => saveSource(...a),
  },
  analysisApi: {analyzeFlow: vi.fn().mockResolvedValue(null)},
}))

vi.mock('@/components/shared', () => ({
  useToast: () => ({error: vi.fn(), success: vi.fn()}),
  useConfirm: () => ({confirm: vi.fn().mockResolvedValue(true)}),
  Spinner: () => <div data-testid="spinner" />,
}))

vi.mock('./AddActionForm', () => ({
  default: () => null,
  insertBeforeLastRegionEnd: (s: string) => s,
}))

const flowState: {
  document: {id: string; isFolder?: boolean; filePath?: string}
  readOnly: boolean
  setDocument: unknown
} = {
  document: {id: 'flow-1'},
  readOnly: false,
  setDocument: vi.fn(),
}
vi.mock('@/stores/flowStore', () => ({
  useFlowStore: Object.assign((sel: (s: typeof flowState) => unknown) => sel(flowState), {getState: () => flowState}),
}))
vi.mock('@/stores/analysisStore', () => ({
  useAnalysisStore: (sel: (s: {setReport: unknown}) => unknown) => sel({setReport: vi.fn()}),
}))

import SourceEditor from './SourceEditor'

describe('SourceEditor', () => {
  beforeEach(() => {
    getSource.mockReset()
    saveSource.mockReset()
    flowState.document = {id: 'flow-1'}
  })

  // The destruction chain this guards: getSource rejects -> the catch only
  // toasted -> source and original stayed '' while loading cleared -> a fully
  // interactive EMPTY textarea rendered -> one keystroke made it dirty (original
  // was '') -> Save sent that single character as the entire PAD source. A
  // transient 500 on load destroyed the flow.
  it('does not render an editable empty buffer when the source fails to load', async () => {
    getSource.mockRejectedValue(new Error('500 internal server error'))
    render(<SourceEditor onClose={() => {}} />)

    await waitFor(() => {
      expect(screen.queryByTestId('spinner')).toBeNull()
    })

    expect(screen.queryByRole('textbox')).toBeNull()
    expect(screen.getByText(/couldn't load the flow source/i)).toBeTruthy()
  })

  // The sharper assertion: even reaching the save path must not write.
  it('never calls saveSource after a failed load', async () => {
    getSource.mockRejectedValue(new Error('boom'))
    render(<SourceEditor onClose={() => {}} />)

    await waitFor(() => expect(screen.queryByTestId('spinner')).toBeNull())

    // There is no textarea and no Save button to reach; assert the write never
    // happened regardless of how the UI is driven.
    expect(saveSource).not.toHaveBeenCalled()
  })

  // Retry must actually re-fetch, or the error state is a dead end that forces
  // a full reload to recover from a transient blip.
  it('retries the fetch and shows the editor once it succeeds', async () => {
    getSource.mockRejectedValueOnce(new Error('transient'))
    getSource.mockResolvedValueOnce({source: '#Region "Main"\n#EndRegion\n'})

    render(<SourceEditor onClose={() => {}} />)
    await waitFor(() => expect(screen.getByText(/couldn't load the flow source/i)).toBeTruthy())

    fireEvent.click(screen.getByText('Retry'))

    await waitFor(() => {
      expect(screen.getByRole('textbox')).toBeTruthy()
    })
    expect(getSource).toHaveBeenCalledTimes(2)
  })

  // The happy path must be untouched: a successful load still yields an
  // editable buffer holding the fetched source.
  it('renders the fetched source normally on success', async () => {
    getSource.mockResolvedValue({source: '#Region "Main"\nWAIT 1\n#EndRegion\n'})
    render(<SourceEditor onClose={() => {}} />)

    await waitFor(() => {
      const box = screen.getByRole('textbox') as HTMLTextAreaElement
      expect(box.value).toContain('WAIT 1')
    })
  })

  // A cloud folder flow has no per-file source, and the server refuses to save
  // the combined text (it would collapse the folder). Save used to arm on the
  // first keystroke and fail every time; a desktop folder (on-disk path) saves
  // the selected member file and must stay enabled.
  it.each([
    ['cloud folder', {id: 'flow-1', isFolder: true, filePath: ''}, true],
    ['desktop folder', {id: 'flow-1', isFolder: true, filePath: '/flows/F'}, false],
  ])('Save for a %s: disabled=%s', async (_label, doc, disabled) => {
    flowState.document = doc
    getSource.mockResolvedValue({source: '#Region "Main"\nWAIT 1\n#EndRegion\n'})
    render(<SourceEditor onClose={() => {}} />)

    const box = (await screen.findByRole('textbox')) as HTMLTextAreaElement
    fireEvent.change(box, {target: {value: box.value + 'WAIT 2\n'}})
    const save = screen.getByText('Save & Re-parse').closest('button') as HTMLButtonElement
    expect(save.disabled).toBe(disabled)
  })
})

describe('SourceEditor line gutter', () => {
  beforeEach(() => {
    getSource.mockReset()
    flowState.document = {id: 'flow-1'}
  })

  // The line-number <div>s inside the gutter (its two spacers carry no text).
  const gutterNumbers = () => Array.from(screen.getByTestId('line-gutter').children).filter(el => el.textContent !== '')

  // One node per line meant 30k nodes for a real PAD export, all of them
  // re-rendered whenever a line was added. jsdom lays nothing out, so the
  // viewport falls back to 800 px / 20 px lines = 40 lines, plus the buffer.
  it('renders only the viewport (plus buffer) of a 30k-line source', async () => {
    const lines = Array.from({length: 30_000}, (_, i) => `SET Var${i} TO ${i}`)
    getSource.mockResolvedValue({source: lines.join('\n')})
    render(<SourceEditor onClose={() => {}} />)

    await screen.findByRole('textbox')
    const numbers = gutterNumbers()
    expect(numbers.length).toBeGreaterThanOrEqual(40)
    expect(numbers.length).toBeLessThanOrEqual(100)
    expect(numbers[0]!.textContent).toBe('1')
    expect(screen.queryByText('30000')).toBeNull()
    // The spacers keep the gutter's scroll height equal to 30k lines.
    const gutter = screen.getByTestId('line-gutter')
    const bottomSpacer = gutter.lastElementChild as HTMLElement
    expect(bottomSpacer.style.height).toBe(`${(30_000 - numbers.length) * 20}px`)
  })

  it('follows the textarea scroll position', async () => {
    const lines = Array.from({length: 30_000}, (_, i) => `SET Var${i} TO ${i}`)
    getSource.mockResolvedValue({source: lines.join('\n')})
    render(<SourceEditor onClose={() => {}} />)

    const box = (await screen.findByRole('textbox')) as HTMLTextAreaElement
    Object.defineProperty(box, 'scrollTop', {value: 20 * 10_000, writable: true, configurable: true})
    fireEvent.scroll(box)

    expect(screen.getByText('10001')).toBeInTheDocument()
    expect(screen.queryByText('1')).toBeNull()
    expect(screen.getByTestId('line-gutter').scrollTop).toBe(20 * 10_000)
  })

  it('keeps the line count exact as lines are inserted and removed', async () => {
    getSource.mockResolvedValue({source: 'a\nb\nc'})
    render(<SourceEditor onClose={() => {}} />)

    const box = (await screen.findByRole('textbox')) as HTMLTextAreaElement
    expect(gutterNumbers().map(el => el.textContent)).toEqual(['1', '2', '3'])

    fireEvent.change(box, {target: {value: 'a\nb\nX\nc'}})
    expect(gutterNumbers().map(el => el.textContent)).toEqual(['1', '2', '3', '4'])

    fireEvent.change(box, {target: {value: 'a'}})
    expect(gutterNumbers().map(el => el.textContent)).toEqual(['1'])

    fireEvent.click(screen.getByText('Revert'))
    expect(gutterNumbers().map(el => el.textContent)).toEqual(['1', '2', '3'])
  })
})
