import {useMemo} from 'react'
import {createAdapter} from '@/platform/adapters'
import type {PlatformAdapter, NotificationOptions} from '@/platform/types'

// Module-level singleton — the adapter is stateless (env-detection at most),
// so one instance is shared by every caller instead of rebuilding it on each
// use. Mirrors the precedent in TitleBar.tsx, but centralized so notifications
// and other imperative platform calls share one instance.
let sharedAdapter: PlatformAdapter | null = null

export function getPlatform(): PlatformAdapter {
  if (!sharedAdapter) sharedAdapter = createAdapter()
  return sharedAdapter
}

// Test-only hooks. setPlatformForTest injects a fake adapter so tests don't
// touch the real browser Notification API; resetPlatformForTest clears the
// cache so a subsequent getPlatform() rebuilds from createAdapter().
export function setPlatformForTest(adapter: PlatformAdapter | null): void {
  sharedAdapter = adapter
}

export function resetPlatformForTest(): void {
  sharedAdapter = null
}

/**
 * Access the shared platform adapter. Notifications and other imperative
 * platform calls are also exposed as standalone helpers below so non-React
 * modules (Zustand stores, services) can use them without a hook.
 */
export function usePlatform(): PlatformAdapter {
  // The adapter is a stable singleton; useMemo is belt-and-suspenders so React
  // consumers get a referentially-stable value across renders.
  return useMemo(() => getPlatform(), [])
}

// Notifications are only useful when the user isn't already looking at the app
// — firing one while the window is focused would spam. This gate is applied in
// one place so every call site gets consistent behavior.
export async function notifyIfBackground(options: NotificationOptions): Promise<void> {
  if (typeof document !== 'undefined' && document.visibilityState === 'visible') return
  try {
    await getPlatform().showNotification(options)
  } catch (err) {
    // Notifications are best-effort; never let a failure here disrupt the
    // action that triggered them.
    if (import.meta.env.DEV) console.warn('showNotification failed', err)
  }
}
