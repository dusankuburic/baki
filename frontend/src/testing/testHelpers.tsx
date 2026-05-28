import { render, type RenderOptions } from '@testing-library/react'
import { type ReactElement } from 'react'
import { vi } from 'vitest'

/**
 * Render a component with no extra providers.
 * Use this for components that have no external dependencies.
 */
export function renderComponent(ui: ReactElement, options?: RenderOptions) {
  return render(ui, options)
}

// ---------------------------------------------------------------------------
// Platform helpers
// ---------------------------------------------------------------------------

/**
 * Simulate running inside Tauri desktop by injecting __TAURI__ on window.
 * Returns a cleanup function that removes it.
 */
export function simulateTauri(): () => void {
  ;(window as any).__TAURI__ = {}
  return () => delete (window as any).__TAURI__
}

/**
 * Simulate running in a plain web browser (the default).
 */
export function simulateWeb(): void {
  delete (window as any).__TAURI__
}

// ---------------------------------------------------------------------------
// Fetch mock helpers
// ---------------------------------------------------------------------------

/**
 * Replace global fetch with a vitest mock that resolves with the given payload.
 */
export function mockFetch<T>(payload: T, status = 200) {
  const spy = vi.spyOn(globalThis, 'fetch').mockResolvedValue(
    new Response(JSON.stringify(payload), {
      status,
      headers: { 'Content-Type': 'application/json' },
    })
  )
  return spy
}

/**
 * Replace global fetch with a mock that rejects.
 */
export function mockFetchError(message = 'Network error') {
  return vi.spyOn(globalThis, 'fetch').mockRejectedValue(new Error(message))
}
