import {useMemo} from 'react'
import {createAdapter} from '@/platform/adapters'
import type {PlatformAdapter, NotificationOptions} from '@/platform/types'

let sharedAdapter: PlatformAdapter | null = null

export function getPlatform(): PlatformAdapter {
  if (!sharedAdapter) sharedAdapter = createAdapter()
  return sharedAdapter
}

export function setPlatformForTest(adapter: PlatformAdapter | null): void {
  sharedAdapter = adapter
}

export function resetPlatformForTest(): void {
  sharedAdapter = null
}

export function usePlatform(): PlatformAdapter {
  return useMemo(() => getPlatform(), [])
}

export async function notifyIfBackground(options: NotificationOptions): Promise<void> {
  if (typeof document !== 'undefined' && document.visibilityState === 'visible') return
  try {
    await getPlatform().showNotification(options)
  } catch (err) {
    if (import.meta.env.DEV) console.warn('showNotification failed', err)
  }
}
