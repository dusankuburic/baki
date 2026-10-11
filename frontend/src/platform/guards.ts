
export function isTauri(): boolean {
  return '__TAURI_INTERNALS__' in window || '__TAURI__' in window
}

export function isWeb(): boolean {
  return !isTauri()
}

export type PlatformType = 'tauri' | 'web' | 'unknown'

export function getPlatformType(): PlatformType {
  if (isTauri()) return 'tauri'
  if (isWeb()) return 'web'
  return 'unknown'
}

export interface PlatformCapabilities {
  fileSystem: boolean
  nativeDialogs: boolean
  clipboard: boolean
  notifications: boolean
  systemTray: boolean
  nativeWindow: boolean
}

export function getPlatformCapabilities(): PlatformCapabilities {
  if (isTauri()) {
    return {
      fileSystem: true,
      nativeDialogs: true,
      clipboard: true,
      notifications: true,
      systemTray: true,
      nativeWindow: true,
    }
  }

  return {
    fileSystem: false,
    nativeDialogs: false,
    clipboard: true,
    notifications: true,
    systemTray: false,
    nativeWindow: false,
  }
}
