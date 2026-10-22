
import type {PlatformAdapter, BackendConfig, FileOpenOptions, FileSaveOptions, NotificationOptions} from '../types'
import {logger} from '@/lib/logger'

export class WebAdapter implements PlatformAdapter {
  private config: BackendConfig | null = null

  async getBackendConfig(): Promise<BackendConfig> {
    if (this.config) {
      return this.config
    }

    const apiUrl = import.meta.env.VITE_API_URL || window.location.origin

    let token: string | undefined
    try {
      const res = await fetch(`${apiUrl}/api/local-config`)
      if (res.ok) {
        const data = await res.json()
        token = data.token
      }
    } catch {
    }

    this.config = {
      apiUrl,
      token,
      version: import.meta.env.VITE_APP_VERSION,
    }

    return this.config
  }

  async fileOpen(options: FileOpenOptions): Promise<string | string[] | null> {
    return new Promise(resolve => {
      const input = document.createElement('input')
      input.type = 'file'
      input.multiple = options.multiple || false
      input.style.display = 'none'

      if (options.filters && options.filters.length > 0) {
        const extensions = options.filters.flatMap(f => f.extensions)
        input.accept = extensions.map(ext => `.${ext}`).join(',')
      }

      let settled = false
      const cleanup = () => {
        if (input.parentNode) document.body.removeChild(input)
      }
      const done = (val: string | string[] | null) => {
        if (settled) return
        settled = true
        cleanup()
        clearTimeout(fallbackTimer)
        resolve(val)
      }
      const fallbackTimer = setTimeout(() => done(null), 300_000)

      input.onchange = e => {
        const files = (e.target as HTMLInputElement).files
        if (!files || files.length === 0) {
          done(null)
          return
        }

        const file = files[0]
        if (!file) {
          done(null)
          return
        }
        const maxBytes = 100 * 1024 * 1024
        if (file.size > maxBytes) {
          logger.warn('File exceeds 100MB limit', {size: file.size})
          done(null)
          return
        }
        const reader = new FileReader()
        reader.onload = e => {
          const content = e.target?.result as string
          done(
            JSON.stringify({
              __is_web_upload__: true,
              name: file.name,
              files: {[file.name]: content},
            }),
          )
        }
        reader.onerror = () => {
          logger.warn('FileReader error', {name: file.name})
          done(null)
        }
        reader.readAsText(file)
      }

      input.oncancel = () => done(null)

      document.body.appendChild(input)
      input.click()
    })
  }

  async fileOpenDirectory(): Promise<string | null> {
    return new Promise((resolve, reject) => {
      const input = document.createElement('input')
      input.type = 'file'
      input.webkitdirectory = true
      input.style.display = 'none'

      let settled = false
      const cleanup = () => {
        if (input.parentNode) document.body.removeChild(input)
      }
      const done = (val: string | null) => {
        if (settled) return
        settled = true
        cleanup()
        clearTimeout(fallbackTimer)
        resolve(val)
      }
      const fallbackTimer = setTimeout(() => done(null), 300_000)

      input.onchange = async e => {
        const files = (e.target as HTMLInputElement).files
        if (!files || files.length === 0) {
          done(null)
          return
        }

        const validFiles = Array.from(files).filter(file => {
          const lower = file.name.toLowerCase()
          return lower.endsWith('.txt') || lower.endsWith('.pad')
        })

        if (validFiles.length === 0) {
          logger.warn('No .txt or .pad files found in selected directory', {totalFiles: files.length})
          cleanup()
          clearTimeout(fallbackTimer)
          settled = true
          reject(new Error('No .txt or .pad flow files found in selected folder'))
          return
        }

        const fileMap: Record<string, string> = {}
        let directoryName = ''

        const promises = validFiles.map(file => {
          if (!directoryName && file.webkitRelativePath) {
            directoryName = file.webkitRelativePath.split('/')[0] ?? ''
          }
          const relPath = file.webkitRelativePath
            ? file.webkitRelativePath.split('/').slice(1).join('/') || file.name
            : file.name

          return new Promise<void>(res => {
            const maxBytes = 100 * 1024 * 1024
            if (file.size > maxBytes) {
              logger.warn('Folder member exceeds 100MB limit, skipping', {name: file.name, size: file.size})
              res()
              return
            }
            const reader = new FileReader()
            reader.onload = ev => {
              fileMap[relPath] = ev.target?.result as string
              res()
            }
            reader.onerror = () => {
              logger.warn('Failed to read folder member', {name: file.name})
              res()
            }
            reader.readAsText(file)
          })
        })

        await Promise.all(promises)

        if (Object.keys(fileMap).length === 0) {
          cleanup()
          clearTimeout(fallbackTimer)
          settled = true
          reject(new Error('Failed to read files from selected folder'))
          return
        }

        done(
          JSON.stringify({
            __is_web_upload__: true,
            name: directoryName || 'Uploaded Folder',
            files: fileMap,
          }),
        )
      }

      input.oncancel = () => done(null)

      document.body.appendChild(input)
      input.click()
    })
  }

  async fileSave(_options: FileSaveOptions): Promise<string | null> {
    logger.warn('File save dialogs not supported in web browsers. Use download instead.')
    return null
  }

  async openURL(url: string): Promise<void> {
    window.open(url, '_blank', 'noopener,noreferrer')
  }

  async showNotification(options: NotificationOptions): Promise<void> {
    if ('Notification' in window) {
      if (Notification.permission === 'granted') {
        new Notification(options.title, {
          body: options.body,
          icon: options.icon,
        })
      } else if (Notification.permission !== 'denied') {
        Notification.requestPermission()
          .then(permission => {
            if (permission === 'granted') {
              new Notification(options.title, {
                body: options.body,
                icon: options.icon,
              })
            }
          })
          .catch(() => {})
      }
    } else {
      logger.warn('Notifications not supported in this browser')
    }
  }

  async writeClipboard(text: string): Promise<void> {
    try {
      if (navigator.clipboard && navigator.clipboard.writeText) {
        await navigator.clipboard.writeText(text)
      } else {
        logger.warn('Clipboard API not available')
        throw new Error('Clipboard API not available')
      }
    } catch (error) {
      logger.warn('Failed to write to clipboard:', error)
      throw error
    }
  }

  async minimizeWindow(): Promise<void> {}
  async toggleMaximizeWindow(): Promise<void> {}
  async closeWindow(): Promise<void> {}

  onBackendEvent(): () => void {
    return () => {}
  }
  async restartBackend(): Promise<void> {}
}
