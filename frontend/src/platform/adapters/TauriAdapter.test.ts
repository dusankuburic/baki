import {describe, it, expect, vi, beforeEach, afterEach} from 'vitest'

const invokeMock = vi.fn()
vi.mock('@tauri-apps/api/core', () => ({
  invoke: (...a: unknown[]) => invokeMock(...a),
}))
const listenMock = vi.fn().mockResolvedValue(() => {})
vi.mock('@tauri-apps/api/event', () => ({
  listen: (...a: unknown[]) => listenMock(...a),
}))
vi.mock('@tauri-apps/api/window', () => ({getCurrentWindow: () => ({})}))
vi.mock('@tauri-apps/plugin-dialog', () => ({open: vi.fn(), save: vi.fn()}))
vi.mock('@tauri-apps/plugin-shell', () => ({open: vi.fn()}))

import {TauriAdapter} from './TauriAdapter'

describe('TauriAdapter.getBackendConfig deadline (L9-fe)', () => {
  beforeEach(() => {
    vi.useFakeTimers()
    invokeMock.mockReset()
    listenMock.mockReset()
    invokeMock.mockRejectedValue(new Error('not ready'))
    listenMock.mockResolvedValue(() => {})
  })
  afterEach(() => {
    vi.useRealTimers()
  })

  it('rejects after the deadline when the sidecar never becomes ready', async () => {
    const adapter = new TauriAdapter()
    const p = adapter.getBackendConfig()

    let rejected = false
    p.catch(() => {
      rejected = true
    })
    await vi.advanceTimersByTimeAsync(1_000)
    expect(rejected).toBe(false)

    await vi.advanceTimersByTimeAsync(60_000)
    await expect(p).rejects.toThrow(/did not become ready/i)
  })

  it('resolves as soon as the sidecar signals ready (invoke succeeds on retry)', async () => {
    invokeMock.mockReset()
    invokeMock
      .mockRejectedValueOnce(new Error('not ready'))
      .mockResolvedValueOnce({port: 9999, token: 'tok'})

    const adapter = new TauriAdapter()
    const p = adapter.getBackendConfig()

    const cfg = await vi.advanceTimersByTimeAsync(600)
    void cfg
    const result = await p
    expect(result.apiUrl).toBe('http://localhost:9999')
    expect(result.token).toBe('tok')
  })
})
