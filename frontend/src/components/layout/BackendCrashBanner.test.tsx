import {describe, it, expect, vi, afterEach} from 'vitest'
import {act, render, screen} from '@testing-library/react'
import BackendCrashBanner from './BackendCrashBanner'
import {setPlatformForTest} from '@/hooks/usePlatform'
import {onBackendRestarted} from '@/lib/backendRestart'
import type {BackendEvent, PlatformAdapter} from '@/platform/types'

function fakePlatform() {
  const subs = new Set<(e: BackendEvent) => void>()
  const platform = {
    onBackendEvent: (cb: (e: BackendEvent) => void) => {
      subs.add(cb)
      return () => subs.delete(cb)
    },
    restartBackend: vi.fn(async () => {}),
  } as unknown as PlatformAdapter
  const emit = (e: BackendEvent) => act(() => subs.forEach(cb => cb(e)))
  return {platform, emit}
}

afterEach(() => {
  setPlatformForTest(null)
})

describe('BackendCrashBanner (R-frontend-c7)', () => {
  it('a sidecar that came back after a crash is a new backend: the app moves to it, no reload needed', () => {
    const {platform, emit} = fakePlatform()
    setPlatformForTest(platform)
    const restarted = vi.fn()
    const off = onBackendRestarted(restarted)
    render(<BackendCrashBanner />)

    emit({kind: 'terminated', message: 'exit code 1'})
    expect(screen.getByRole('alertdialog')).toBeInTheDocument()
    emit({kind: 'ready', message: '', config: {apiUrl: 'http://localhost:5123', token: 't', port: 5123}})

    expect(screen.queryByRole('alertdialog')).not.toBeInTheDocument()
    expect(restarted).toHaveBeenCalledTimes(1)
    off()
  })

  it('the first ready of a fresh start is not a restart', () => {
    const {platform, emit} = fakePlatform()
    setPlatformForTest(platform)
    const restarted = vi.fn()
    const off = onBackendRestarted(restarted)
    render(<BackendCrashBanner />)

    emit({kind: 'ready', message: ''})

    expect(restarted).not.toHaveBeenCalled()
    off()
  })
})
