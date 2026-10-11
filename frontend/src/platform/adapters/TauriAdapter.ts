
import {invoke} from '@tauri-apps/api/core'
import {listen} from '@tauri-apps/api/event'
import {open, save} from '@tauri-apps/plugin-dialog'
import {open as shellOpen} from '@tauri-apps/plugin-shell'
import {logger} from '@/lib/logger'
import type {
  PlatformAdapter,
  BackendConfig,
  BackendEvent,
  FileOpenOptions,
  FileSaveOptions,
  NotificationOptions,
} from '../types'

const SIDEKICK_READY_DEADLINE_MS = 60_000

export class TauriAdapter implements PlatformAdapter {
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
          reject(new Error(`backend sidecar did not become ready within ${SIDEKICK_READY_DEADLINE_MS / 1000}s`))
        }

        listen<{port: number; token: string}>('backend-ready', event => {
          finish(toConfig(event.payload))
        })
          .then(fn => {
            unlisten = fn
          })
          .catch(() => {})

        retryTimer = setInterval(() => {
          invoke<{port: number; token: string}>('get_backend_config')
            .then(raw => finish(toConfig(raw)))
            .catch(() => {})
        }, 500)

        deadlineTimer = setTimeout(fail, SIDEKICK_READY_DEADLINE_MS)
      })
    }
  }

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

  async openURL(url: string): Promise<void> {
    try {
      await shellOpen(url)
    } catch (error) {
      logger.warn('Failed to open URL:', error)
      throw error
    }
  }

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

  async writeClipboard(text: string): Promise<void> {
    await navigator.clipboard.writeText(text)
  }

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

  onBackendEvent(cb: (event: BackendEvent) => void): () => void {
    let active = true
    const unlisteners: (() => void)[] = []
    const attach = (name: string, kind: BackendEvent['kind']) => {
      listen<unknown>(name, event => {
        if (!active) return
        const payload = event.payload
        cb({kind, message: typeof payload === 'string' ? payload : ''})
      })
        .then(fn => {
          if (active) unlisteners.push(fn)
          else fn()
        })
        .catch(err => logger.warn(`Failed to listen for ${name}:`, err))
    }
    attach('backend-terminated', 'terminated')
    attach('backend-error', 'error')
    attach('backend-ready', 'ready')
    return () => {
      active = false
      for (const fn of unlisteners) fn()
      unlisteners.length = 0
    }
  }

  async restartBackend(): Promise<void> {
    await invoke('restart_backend')
  }
}
