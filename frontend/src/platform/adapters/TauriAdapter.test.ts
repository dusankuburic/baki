import {describe, it, expect, vi, beforeEach, afterEach} from 'vitest'

// Mock the Tauri APIs: invoke always rejects (sidecar never ready) and listen
// never resolves its registered callback (no 'backend-ready' event).
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
    // Sidecar never ready: invoke rejects; listen registers but never fires.
    invokeMock.mockRejectedValue(new Error('not ready'))
    listenMock.mockResolvedValue(() => {})
  })
  afterEach(() => {
    vi.useRealTimers()
  })

  it('rejects after the deadline when the sidecar never becomes ready', async () => {
    const adapter = new TauriAdapter()
    const p = adapter.getBackendConfig()

    // Not yet rejected right away.
    let rejected = false
    p.catch(() => {
      rejected = true
    })
    await vi.advanceTimersByTimeAsync(1_000)
    expect(rejected).toBe(false)

    // Cross the 60s deadline.
    await vi.advanceTimersByTimeAsync(60_000)
    await expect(p).rejects.toThrow(/did not become ready/i)
  })

  it('resolves as soon as the sidecar signals ready (invoke succeeds on retry)', async () => {
    // First invoke (the eager try) rejects → enters the retry path. Then the
    // periodic retry invoke succeeds, resolving before the deadline.
    invokeMock.mockReset()
    invokeMock
      .mockRejectedValueOnce(new Error('not ready')) // eager try
      .mockResolvedValueOnce({port: 9999, token: 'tok'}) // first retry succeeds

    const adapter = new TauriAdapter()
    const p = adapter.getBackendConfig()

    // Advance past the first 500ms retry tick.
    const cfg = await vi.advanceTimersByTimeAsync(600)
    // advanceTimersByTimeAsync returns the last flushed timer time; resolve.
    void cfg
    const result = await p
    expect(result.apiUrl).toBe('http://localhost:9999')
    expect(result.token).toBe('tok')
  })
})
