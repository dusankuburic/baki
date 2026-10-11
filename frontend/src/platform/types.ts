
export interface BackendConfig {
  apiUrl: string
  token?: string
  version?: string
  port?: number
}

export interface FileOpenOptions {
  filters?: FileFilter[]
  multiple?: boolean
  directory?: boolean
}

export interface FileFilter {
  name: string
  extensions: string[]
}

export interface FileSaveOptions {
  defaultPath?: string
  filters?: FileFilter[]
}

export interface BackendEvent {
  kind: 'terminated' | 'error' | 'ready'
  message: string
}

export interface PlatformAdapter {
  getBackendConfig(): Promise<BackendConfig>

  fileOpen(options: FileOpenOptions): Promise<string | string[] | null>

  fileOpenDirectory(): Promise<string | null>

  fileSave(options: FileSaveOptions): Promise<string | null>

  openURL(url: string): Promise<void>

  showNotification(options: NotificationOptions): Promise<void>

  writeClipboard(text: string): Promise<void>

  minimizeWindow(): Promise<void>

  toggleMaximizeWindow(): Promise<void>

  closeWindow(): Promise<void>

  onBackendEvent(cb: (event: BackendEvent) => void): () => void

  restartBackend(): Promise<void>
}

export interface NotificationOptions {
  title: string
  body: string
  icon?: string
}
