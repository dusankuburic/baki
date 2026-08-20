/**
 * Tauri-specific platform adapter implementation
 */

import {invoke} from '@tauri-apps/api/core'
import {listen} from '@tauri-apps/api/event'
import {open, save} from '@tauri-apps/plugin-dialog'
import {open as shellOpen} from '@tauri-apps/plugin-shell'
import {logger} from '@/lib/logger'
import type {PlatformAdapter, BackendConfig, FileOpenOptions, FileSaveOptions, NotificationOptions} from '../types'

// SIDEKICK_READY_DEADLINE_MS bounds how long getBackendConfig waits for the Go
// sidecar to come up before giving up. Without it a sidecar that never starts
// (crash, misconfig) leaves the app retrying forever on a blank screen. 60s
// covers slow machines + cold starts; a healthy sidecar is ready in <2s.
const SIDEKICK_READY_DEADLINE_MS = 60_000

/**
 * Tauri adapter for desktop-specific operations
 */
export class TauriAdapter implements PlatformAdapter {
  /**
   * Get backend configuration from Tauri sidecar.
   * Falls back to listening for the backend-ready event if the sidecar isn't up yet.
   */
  async getBackendConfig(): Promise<BackendConfig> {
    const toConfig = (raw: {port: number; token: string}): BackendConfig => ({
      apiUrl: `http://localhost:${raw.port}`,
      token: raw.token,
      port: raw.port,
    })

    try {
      const raw = await invoke<{port: number; token: string}>('get_backend_config')
      return toConfig(raw)
    } catch {
      // Sidecar not ready yet. Use BOTH a listener for the 'backend-ready'
      // event AND periodic invoke retries, because listen() is async and the
      // event may fire before the listener is attached.
      //
      // The whole wait is bounded by SIDEKICK_READY_DEADLINE_MS: if the sidecar
      // hasn't become ready by then (it crashed, misconfigured, or died), we
      // stop retrying and reject. Without this deadline the setInterval retried
      // forever and getBackendConfig() never settled, so the app hung on a
      // blank screen with no signal — and the 'backend-ready' listener leaked.
      return new Promise((resolve, reject) => {
        let resolved = false
        let unlisten: (() => void) | null = null
        let retryTimer: ReturnType<typeof setInterval> | null = null
        let deadlineTimer: ReturnType<typeof setTimeout> | null = null

        const finish = (cfg: BackendConfig) => {
          if (resolved) return
          resolved = true
          if (unlisten) unlisten()
          if (retryTimer) clearInterval(retryTimer)
          if (deadlineTimer) clearTimeout(deadlineTimer)
          resolve(cfg)
        }

        const fail = () => {
          if (resolved) return
          resolved = true
          if (unlisten) unlisten()
          if (retryTimer) clearInterval(retryTimer)
          logger.error(
            `Backend sidecar did not become ready within ${SIDEKICK_READY_DEADLINE_MS / 1000}s — ` +
              'check the sidecar logs/config; the app cannot start without it.',
          )
          reject(
            new Error(
              `backend sidecar did not become ready within ${SIDEKICK_READY_DEADLINE_MS / 1000}s`,
            ),
          )
        }

        // Listen for the event
        listen<{port: number; token: string}>('backend-ready', event => {
          finish(toConfig(event.payload))
        })
          .then(fn => {
            unlisten = fn
          })
          .catch(() => {})

        // Also retry invoke every 500ms in case the event was missed
        retryTimer = setInterval(() => {
          invoke<{port: number; token: string}>('get_backend_config')
            .then(raw => finish(toConfig(raw)))
            .catch(() => {})
        }, 500)

        // Hard deadline so a dead sidecar surfaces as a failure instead of an
        // infinite spin. Generous: covers slow machines + cold sidecar starts.
        deadlineTimer = setTimeout(fail, SIDEKICK_READY_DEADLINE_MS)
      })
    }
  }

  /**
   * Open file dialog using Tauri native dialog
   */
  async fileOpen(options: FileOpenOptions): Promise<string | string[] | null> {
    try {
      const result = await open({
        multiple: options.multiple || false,
        directory: options.directory || false,
        filters: options.filters?.map(filter => ({
          name: filter.name,
          extensions: filter.extensions,
        })),
      })

      return result || null
    } catch (error) {
      logger.warn('File open failed:', error)
      return null
    }
  }

  /**
   * Open directory dialog
   */
  async fileOpenDirectory(): Promise<string | null> {
    try {
      const result = await open({directory: true})
      if (!result) return null
      return Array.isArray(result) ? result[0] : result
    } catch (error) {
      logger.warn('Directory open failed:', error)
      return null
    }
  }

  /**
   * Save file dialog using Tauri native dialog
   */
  async fileSave(options: FileSaveOptions): Promise<string | null> {
    try {
      const result = await save({
        defaultPath: options.defaultPath,
        filters: options.filters?.map(f => ({name: f.name, extensions: f.extensions})),
      })
      return result || null
    } catch (error) {
      logger.warn('File save failed:', error)
      return null
    }
  }

  /**
   * Reveal file in system file explorer
   */
  async fileReveal(path: string): Promise<void> {
    try {
      await shellOpen(path)
    } catch (error) {
      logger.warn('Failed to reveal file:', error)
      throw error
    }
  }

  /**
   * Open URL in default browser
   */
  async openURL(url: string): Promise<void> {
    try {
      await shellOpen(url)
    } catch (error) {
      logger.warn('Failed to open URL:', error)
      throw error
    }
  }

  /**
   * Show system notification via browser Notifications API (available in Tauri webview)
   */
  async showNotification(options: NotificationOptions): Promise<void> {
    if (!('Notification' in window)) return

    if (Notification.permission === 'granted') {
      new Notification(options.title, {body: options.body, icon: options.icon})
    } else if (Notification.permission !== 'denied') {
      const permission = await Notification.requestPermission()
      if (permission === 'granted') {
        new Notification(options.title, {body: options.body, icon: options.icon})
      }
    }
  }

  /**
   * Read clipboard using browser Clipboard API (available in Tauri webview)
   */
  async readClipboard(): Promise<string> {
    try {
      return await navigator.clipboard.readText()
    } catch {
      return ''
    }
  }

  /**
   * Write to clipboard using browser Clipboard API (available in Tauri webview)
   */
  async writeClipboard(text: string): Promise<void> {
    await navigator.clipboard.writeText(text)
  }

  // Window controls dynamically import @tauri-apps/api/window (the heaviest
  // Tauri module — it also pulls in dpi.js). A static import would drag ~70 kB
  // into the eagerly-loaded web bundle where these controls are never used;
  // dynamic import matches the pattern in useAppShortcuts/useFileDrop.
  async minimizeWindow(): Promise<void> {
    const {getCurrentWindow} = await import('@tauri-apps/api/window')
    await getCurrentWindow().minimize()
  }

  async toggleMaximizeWindow(): Promise<void> {
    const {getCurrentWindow} = await import('@tauri-apps/api/window')
    await getCurrentWindow().toggleMaximize()
  }

  async closeWindow(): Promise<void> {
    const {getCurrentWindow} = await import('@tauri-apps/api/window')
    await getCurrentWindow().close()
  }
}
