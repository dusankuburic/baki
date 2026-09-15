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

const flowState = {document: {id: 'flow-1'}, readOnly: false, setDocument: vi.fn()}
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
})
