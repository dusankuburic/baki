import {describe, it, expect, vi, beforeEach} from 'vitest'
import {render, screen, fireEvent, waitFor} from '@testing-library/react'
import {OperationsSection} from './OperationsSection'
import {ToastProvider} from '@/components/shared/Toast'

const systemHealth = vi.fn()
const ppStatus = vi.fn()
const triggerScannerScan = vi.fn()
const triggerIngesterIngest = vi.fn()

vi.mock('@/api/admin', () => ({
  adminApi: {
    systemHealth: (...a: unknown[]) => systemHealth(...a),
    ppStatus: (...a: unknown[]) => ppStatus(...a),
    triggerScannerScan: (...a: unknown[]) => triggerScannerScan(...a),
    triggerIngesterIngest: (...a: unknown[]) => triggerIngesterIngest(...a),
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
    ppStatus.mockResolvedValue({connected: true})
    triggerScannerScan.mockResolvedValue({started: true})
    triggerIngesterIngest.mockResolvedValue({started: true})
  })

  it('loads health + connector status on first visibility and renders per-subsystem tiles', async () => {
    renderSection()
    expect(await screen.findByText('Database')).toBeInTheDocument()
    expect(screen.getByText('Blob storage')).toBeInTheDocument()
    expect(screen.getByText('Redis')).toBeInTheDocument()
    expect(screen.getByText('Power Platform')).toBeInTheDocument()
    expect(screen.getByText(/Overall: degraded/)).toBeInTheDocument()
    // Error detail on hover title.
    expect(screen.getByTitle('connection refused')).toBeInTheDocument()
  })

  it('triggers a governance scan on click', async () => {
    renderSection()
    await screen.findByText('Database')
    fireEvent.click(screen.getByRole('button', {name: /run governance scan/i}))
    await waitFor(() => expect(triggerScannerScan).toHaveBeenCalledTimes(1))
  })

  it('triggers a cloud ingest on click', async () => {
    renderSection()
    await screen.findByText('Database')
    fireEvent.click(screen.getByRole('button', {name: /ingest cloud flows/i}))
    await waitFor(() => expect(triggerIngesterIngest).toHaveBeenCalledTimes(1))
  })

  it('refresh re-fetches health', async () => {
    renderSection()
    await screen.findByText('Database')
    fireEvent.click(screen.getByRole('button', {name: /refresh/i}))
    await waitFor(() => expect(systemHealth).toHaveBeenCalledTimes(2))
  })

  it('renders unknown-state tiles while health has not loaded', async () => {
    systemHealth.mockReturnValue(new Promise(() => {})) // pending
    ppStatus.mockReturnValue(new Promise(() => {})) // pending too
    renderSection()
    expect(await screen.findByText('Database')).toBeInTheDocument()
    expect(screen.getAllByText('unknown').length).toBeGreaterThanOrEqual(4)
  })
})
