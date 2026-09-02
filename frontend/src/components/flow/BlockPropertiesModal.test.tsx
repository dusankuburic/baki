import {describe, it, expect, vi, beforeEach} from 'vitest'
import {render, screen, fireEvent, waitFor} from '@testing-library/react'
import BlockPropertiesModal from './BlockPropertiesModal'
import {useFlowStore} from '@/stores/flowStore'
import {ToastProvider} from '@/components/shared/Toast'
import type {Block} from '@/types'

const updateBlockProperties = vi.fn()
const analyzeFlow = vi.fn()

vi.mock('@/api', () => ({
  flowApi: {
    updateBlockProperties: (...a: unknown[]) => updateBlockProperties(...a),
  },
  analysisApi: {
    analyzeFlow: (...a: unknown[]) => analyzeFlow(...a),
  },
}))

const block: Block = {
  subflowId: 'sf1',
  id: 'b1',
  name: 'Invoke Url',
  type: 'ACTION',
  rawType: 'HTTPClient.InvokeUrl',
  indent: 0,
  lineNumber: 3,
  children: [],
  properties: {Url: 'https://x', Method: 'GET', _output: 'Resp'},
  variables: [],
}

function renderModal(b: Block = block) {
  return render(
    <ToastProvider>
      <BlockPropertiesModal block={b} onClose={vi.fn()} />
    </ToastProvider>,
  )
}

beforeEach(() => {
  vi.clearAllMocks()
  analyzeFlow.mockResolvedValue(null)
  useFlowStore.setState({document: {id: 'flow-1', name: 'F'} as never})
})

describe('BlockPropertiesModal (R3-2)', () => {
  it('renders editable properties, hiding parser-derived keys', () => {
    renderModal()
    expect(screen.getByLabelText('Value for Url')).toHaveValue('https://x')
    expect(screen.getByLabelText('Value for Method')).toHaveValue('GET')
    expect(screen.queryByLabelText('Value for _output')).not.toBeInTheDocument()
  })

  it('Save is disabled until a value changes; label reflects the change count', () => {
    renderModal()
    const save = screen.getByRole('button', {name: /^Save/})
    expect(save).toBeDisabled()
    fireEvent.change(screen.getByLabelText('Value for Url'), {target: {value: 'https://y'}})
    expect(screen.getByRole('button', {name: 'Save 1 change'})).toBeEnabled()
  })

  it('sends only the changed keys', async () => {
    updateBlockProperties.mockResolvedValue({document: {id: 'flow-1', name: 'F'}})
    renderModal()
    fireEvent.change(screen.getByLabelText('Value for Url'), {target: {value: 'https://y'}})
    fireEvent.click(screen.getByRole('button', {name: 'Save 1 change'}))
    await waitFor(() =>
      expect(updateBlockProperties).toHaveBeenCalledWith('flow-1', 'b1', {Url: 'https://y'}),
    )
    // Method was untouched — not in the payload.
  })

  it('shows an empty state for blocks without editable properties', () => {
    renderModal({...block, properties: {_output: 'x'}})
    expect(screen.getByText(/no editable properties/i)).toBeInTheDocument()
  })

  it('surfaces save failures without closing', async () => {
    updateBlockProperties.mockRejectedValue(new Error('boom'))
    renderModal()
    fireEvent.change(screen.getByLabelText('Value for Method'), {target: {value: 'POST'}})
    fireEvent.click(screen.getByRole('button', {name: 'Save 1 change'}))
    await waitFor(() => expect(updateBlockProperties).toHaveBeenCalled())
    // Modal still open (failure path does not call onClose).
    expect(screen.getByLabelText('Value for Method')).toBeInTheDocument()
  })
})
