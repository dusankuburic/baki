/**
 * Platform detection utilities for Tauri vs Web deployment
 */

/**
 * Check if running in Tauri desktop environment
 */
export function isTauri(): boolean {
  return '__TAURI_INTERNALS__' in window || '__TAURI__' in window;
}

/**
 * Check if running in web browser environment
 */
export function isWeb(): boolean {
  return !isTauri();
}

/**
 * Get current platform type
 */
export type PlatformType = 'tauri' | 'web' | 'unknown';

export function getPlatformType(): PlatformType {
  if (isTauri()) return 'tauri';
  if (isWeb()) return 'web';
  return 'unknown';
}

/**
 * Platform-specific feature detection
 */
export interface PlatformCapabilities {
  fileSystem: boolean;
  nativeDialogs: boolean;
  clipboard: boolean;
  notifications: boolean;
  systemTray: boolean;
  // nativeWindow: native menu bar / title bar / window controls (minimize,
  // maximize, close, fullscreen) — only available in the Tauri shell, which
  // owns the OS window; the web build renders in a normal browser tab.
  nativeWindow: boolean;
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
    };
  }

  return {
    fileSystem: false,
    nativeDialogs: false,
    clipboard: true, // Clipboard API available in browsers
    notifications: true, // Notifications API available in browsers
    systemTray: false,
    nativeWindow: false,
  };
}