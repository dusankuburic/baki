import {describe, it, expect, vi, beforeEach} from 'vitest'
import {render, screen, fireEvent, waitFor} from '@testing-library/react'
import {OperationsSection} from './OperationsSection'
import {ToastProvider} from '@/components/shared/Toast'
import {ApiError} from '@/api/client'

const systemHealth = vi.fn()
const triggerScannerScan = vi.fn()

vi.mock('@/api/admin', () => ({
  adminApi: {
    systemHealth: (...a: unknown[]) => systemHealth(...a),
    triggerScannerScan: (...a: unknown[]) => triggerScannerScan(...a),
  },
}))

function renderSection() {
  return render(
    <ToastProvider>
      <OperationsSection visible />
    </ToastProvider>,
  )
}

describe('OperationsSection (R2-6)', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    systemHealth.mockResolvedValue({
      database: {status: 'ok'},
      blob: {status: 'skipped'},
      redis: {status: 'error', error: 'connection refused'},
      overall: 'degraded',
    })
    triggerScannerScan.mockResolvedValue({started: true})
  })

  it('loads health on first visibility and renders per-subsystem tiles', async () => {
    renderSection()
    expect(await screen.findByText('Database')).toBeInTheDocument()
    expect(screen.getByText('Blob storage')).toBeInTheDocument()
    expect(screen.getByText('Redis')).toBeInTheDocument()
    expect(screen.getByText(/Overall: degraded/)).toBeInTheDocument()
    expect(screen.getByTitle('connection refused')).toBeInTheDocument()
  })

  it('triggers a governance scan on click', async () => {
    renderSection()
    await screen.findByText('Database')
    fireEvent.click(screen.getByRole('button', {name: /run governance scan/i}))
    await waitFor(() => expect(triggerScannerScan).toHaveBeenCalledTimes(1))
  })

  it('says a scan is already running when the server reports one (409 SCAN_ALREADY_RUNNING)', async () => {
    triggerScannerScan.mockRejectedValue(
      new ApiError('a governance scan is already running', 409, 'SCAN_ALREADY_RUNNING'),
    )
    renderSection()
    await screen.findByText('Database')
    fireEvent.click(screen.getByRole('button', {name: /run governance scan/i}))
    expect(await screen.findByText(/already running/i)).toBeInTheDocument()
    expect(screen.queryByText('Trigger failed')).not.toBeInTheDocument()
  })

  it('says the scan could not start when the server cannot start one (503 SCAN_UNAVAILABLE)', async () => {
    triggerScannerScan.mockRejectedValue(new ApiError('internal server error', 503, 'SCAN_UNAVAILABLE'))
    renderSection()
    await screen.findByText('Database')
    fireEvent.click(screen.getByRole('button', {name: /run governance scan/i}))
    expect(await screen.findByText('Trigger failed')).toBeInTheDocument()
    expect(screen.getByText(/could not start/i)).toBeInTheDocument()
    expect(screen.queryByText(/internal server error/i)).not.toBeInTheDocument()
  })

  it('refresh re-fetches health', async () => {
    renderSection()
    await screen.findByText('Database')
    fireEvent.click(screen.getByRole('button', {name: /refresh/i}))
    await waitFor(() => expect(systemHealth).toHaveBeenCalledTimes(2))
  })

  it('renders unknown-state tiles while health has not loaded', async () => {
    systemHealth.mockReturnValue(new Promise(() => {}))
    renderSection()
    expect(await screen.findByText('Database')).toBeInTheDocument()
    expect(screen.getAllByText('unknown').length).toBeGreaterThanOrEqual(3)
  })
})
