import {render, type RenderOptions} from '@testing-library/react'
import {type ReactElement} from 'react'
import {vi} from 'vitest'
import type {UnknownEnumValue} from '@/types'

export function renderComponent(ui: ReactElement, options?: RenderOptions) {
  return render(ui, options)
}

interface TauriWindow extends Window {
  __TAURI__?: Record<string, unknown>
}

export function simulateTauri(): () => void {
  ;(window as TauriWindow).__TAURI__ = {}
  return () => delete (window as TauriWindow).__TAURI__
}

export function simulateWeb(): void {
  delete (window as TauriWindow).__TAURI__
}

export function mockFetch<T>(payload: T, status = 200) {
  const spy = vi.spyOn(globalThis, 'fetch').mockResolvedValue(
    new Response(JSON.stringify(payload), {
      status,
      headers: {'Content-Type': 'application/json'},
    }),
  )
  return spy
}

export function mockFetchError(message = 'Network error') {
  return vi.spyOn(globalThis, 'fetch').mockRejectedValue(new Error(message))
}

export function unknownValue(raw: string): UnknownEnumValue {
  return raw as UnknownEnumValue
}
