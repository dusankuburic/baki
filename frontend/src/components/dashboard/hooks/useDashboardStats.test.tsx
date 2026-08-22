import {describe, it, expect, vi, beforeEach} from 'vitest'
import {renderHook, waitFor, act} from '@testing-library/react'
import {useDashboardStats} from './useDashboardStats'
import {ToastProvider} from '@/components/shared/Toast'

const getDashboard = vi.fn()
const toastError = vi.fn()

vi.mock('@/api', () => ({
  analysisApi: {
    getDashboard: (...a: unknown[]) => getDashboard(...a),
  },
}))

// useDashboardStats calls useToast for background-refresh-failure toasts.
vi.mock('@/components/shared', async () => {
  const actual = await vi.importActual<typeof import('@/components/shared')>('@/components/shared')
  return {
    ...actual,
    useToast: () => ({success: vi.fn(), error: toastError}),
  }
})

function wrapper({children}: {children: React.ReactNode}) {
  return <ToastProvider>{children}</ToastProvider>
}

const populated = {
  totalFlowsAnalyzed: 3,
  totalFindings: 5,
  findingsBySeverity: {error: 1},
  findingsByCategory: {},
  findingsByRule: {},
  avgHealthScore: 70,
  topProblemFlows: [],
}

beforeEach(() => {
  vi.clearAllMocks()
  getDashboard.mockReset()
  toastError.mockReset()
})

describe('useDashboardStats', () => {
  it('does not fetch when the capability is not loaded yet', () => {
    renderHook(
      ({enabled, isLoaded}: {enabled: boolean; isLoaded: boolean}) => useDashboardStats(enabled, isLoaded, null),
      {
        initialProps: {enabled: true, isLoaded: false},
        wrapper,
      },
    )
    expect(getDashboard).not.toHaveBeenCalled()
  })

  it('does not fetch when session analytics are disabled', () => {
    renderHook(
      ({enabled, isLoaded}: {enabled: boolean; isLoaded: boolean}) => useDashboardStats(enabled, isLoaded, null),
      {
        initialProps: {enabled: false, isLoaded: true},
        wrapper,
      },
    )
    expect(getDashboard).not.toHaveBeenCalled()
  })

  it('a stale response never overwrites a newer one (reqId race guard)', async () => {
    // First (slow) response resolves AFTER the second (fast) one. Without the
    // per-request id guard the stale data would clobber the fresh data.
    getDashboard
      .mockReturnValueOnce(new Promise(r => setTimeout(() => r({totalFlowsAnalyzed: 1}), 50)))
      .mockResolvedValueOnce({totalFlowsAnalyzed: 99})

    const {result} = renderHook(
      ({enabled, isLoaded}: {enabled: boolean; isLoaded: boolean}) => useDashboardStats(enabled, isLoaded, null),
      {initialProps: {enabled: true, isLoaded: true}, wrapper},
    )

    // Mount fired the slow refresh (#1); now fire a fast refresh (#2) that must
    // win, then let the slow one resolve afterwards.
    await act(async () => {
      result.current.refresh()
    })
    await waitFor(() => expect(result.current.stats?.totalFlowsAnalyzed).toBe(99))

    // Wait out the slow first response; it must not have overwritten 99.
    await new Promise(r => setTimeout(r, 80))
    expect(result.current.stats?.totalFlowsAnalyzed).toBe(99)
  })

  it('a background refresh failure toasts instead of replacing good data', async () => {
    getDashboard.mockResolvedValueOnce(populated)

    const {result} = renderHook(
      ({enabled, isLoaded}: {enabled: boolean; isLoaded: boolean}) => useDashboardStats(enabled, isLoaded, null),
      {initialProps: {enabled: true, isLoaded: true}, wrapper},
    )
    await waitFor(() => expect(result.current.stats).toEqual(populated))
    expect(result.current.error).toBeNull()

    // A background refresh that fails must NOT set error (wiping the good
    // data) — it should toast instead.
    getDashboard.mockRejectedValueOnce(new Error('transient'))
    await act(async () => {
      result.current.refresh(true)
      await waitFor(() => expect(toastError).toHaveBeenCalled())
    })

    expect(result.current.stats).toEqual(populated)
    expect(result.current.error).toBeNull()
  })

  it('the FIRST fetch failure sets the error state (no good data to keep)', async () => {
    getDashboard.mockRejectedValueOnce(new Error('boom'))
    const {result} = renderHook(
      ({enabled, isLoaded}: {enabled: boolean; isLoaded: boolean}) => useDashboardStats(enabled, isLoaded, null),
      {initialProps: {enabled: true, isLoaded: true}, wrapper},
    )
    await waitFor(() => expect(result.current.error).toBe('boom'))
    expect(result.current.stats).toBeNull()
    expect(toastError).not.toHaveBeenCalled()
  })
})
