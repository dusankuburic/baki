/**
 * Platform abstraction types and interfaces
 */

/**
 * Backend configuration interface
 */
export interface BackendConfig {
  apiUrl: string;
  token?: string;
  version?: string;
  port?: number;
}

/**
 * File open options for platform-specific file dialogs
 */
export interface FileOpenOptions {
  filters?: FileFilter[];
  multiple?: boolean;
  directory?: boolean;
}

/**
 * File filter for file dialogs
 */
export interface FileFilter {
  name: string;
  extensions: string[];
}

/**
 * File save options for platform-specific file save dialogs
 */
export interface FileSaveOptions {
  defaultPath?: string;
  filters?: FileFilter[];
}

/**
 * Platform adapter interface for abstracting platform-specific operations
 */
export interface PlatformAdapter {
  /**
   * Get backend configuration (port, token, etc.)
   */
  getBackendConfig(): Promise<BackendConfig>;

  /**
   * Open file dialog and return selected file path(s)
   */
  fileOpen(options: FileOpenOptions): Promise<string | string[] | null>;

  /**
   * Open directory dialog and return selected directory path
   */
  fileOpenDirectory(): Promise<string | null>;

  /**
   * Save file dialog and return selected file path
   */
  fileSave(options: FileSaveOptions): Promise<string | null>;

  /**
   * Reveal file in file explorer/finder
   */
  fileReveal(path: string): Promise<void>;

  /**
   * Open URL in default browser
   */
  openURL(url: string): Promise<void>;

  /**
   * Show notification
   */
  showNotification(options: NotificationOptions): Promise<void>;

  /**
   * Read clipboard content
   */
  readClipboard(): Promise<string>;

  /**
   * Write to clipboard
   */
  writeClipboard(text: string): Promise<void>;

  /**
   * Minimize the application window (desktop only; no-op on web).
   */
  minimizeWindow(): Promise<void>;

  /**
   * Toggle maximize/restore the application window (desktop only; no-op on web).
   */
  toggleMaximizeWindow(): Promise<void>;

  /**
   * Close the application window (desktop only; no-op on web).
   */
  closeWindow(): Promise<void>;
}

/**
 * Notification options
 */
export interface NotificationOptions {
  title: string;
  body: string;
  icon?: string;
}

/**
 * Platform detection result
 */
export interface PlatformInfo {
  type: 'tauri' | 'web';
  capabilities: PlatformCapabilities;
}

/**
 * Platform capabilities
 */
export interface PlatformCapabilities {
  fileSystem: boolean;
  nativeDialogs: boolean;
  clipboard: boolean;
  notifications: boolean;
  systemTray: boolean;
}