/**
 * Tauri-specific platform adapter implementation
 */

import { invoke } from '@tauri-apps/api/core';
import { listen } from '@tauri-apps/api/event';
import { getCurrentWindow } from '@tauri-apps/api/window';
import { open, save } from '@tauri-apps/plugin-dialog';
import { open as shellOpen } from '@tauri-apps/plugin-shell';
import type {
  PlatformAdapter,
  BackendConfig,
  FileOpenOptions,
  FileSaveOptions,
  NotificationOptions,
} from '../types';

/**
 * Tauri adapter for desktop-specific operations
 */
export class TauriAdapter implements PlatformAdapter {
  /**
   * Get backend configuration from Tauri sidecar.
   * Falls back to listening for the backend-ready event if the sidecar isn't up yet.
   */
  async getBackendConfig(): Promise<BackendConfig> {
    const toConfig = (raw: { port: number; token: string }): BackendConfig => ({
      apiUrl: `http://localhost:${raw.port}`,
      token: raw.token,
      port: raw.port,
    });

    try {
      const raw = await invoke<{ port: number; token: string }>('get_backend_config');
      return toConfig(raw);
    } catch {
      return new Promise((resolve) => {
        listen<{ port: number; token: string }>('backend-ready', (event) => {
          resolve(toConfig(event.payload));
        });
      });
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
      });

      return result || null;
    } catch (error) {
      console.error('File open failed:', error);
      return null;
    }
  }

  /**
   * Open directory dialog
   */
  async fileOpenDirectory(): Promise<string | null> {
    try {
      const result = await open({ directory: true });
      if (!result) return null;
      return Array.isArray(result) ? result[0] : result;
    } catch (error) {
      console.error('Directory open failed:', error);
      return null;
    }
  }

  /**
   * Save file dialog using Tauri native dialog
   */
  async fileSave(options: FileSaveOptions): Promise<string | null> {
    try {
      const result = await save({
        defaultPath: options.defaultPath,
        filters: options.filters?.map(f => ({ name: f.name, extensions: f.extensions })),
      });
      return result || null;
    } catch (error) {
      console.error('File save failed:', error);
      return null;
    }
  }

  /**
   * Reveal file in system file explorer
   */
  async fileReveal(path: string): Promise<void> {
    try {
      await shellOpen(path);
    } catch (error) {
      console.error('Failed to reveal file:', error);
      throw error;
    }
  }

  /**
   * Open URL in default browser
   */
  async openURL(url: string): Promise<void> {
    try {
      await shellOpen(url);
    } catch (error) {
      console.error('Failed to open URL:', error);
      throw error;
    }
  }

  /**
   * Show system notification via browser Notifications API (available in Tauri webview)
   */
  async showNotification(options: NotificationOptions): Promise<void> {
    if (!('Notification' in window)) return;

    if (Notification.permission === 'granted') {
      new Notification(options.title, { body: options.body, icon: options.icon });
    } else if (Notification.permission !== 'denied') {
      const permission = await Notification.requestPermission();
      if (permission === 'granted') {
        new Notification(options.title, { body: options.body, icon: options.icon });
      }
    }
  }

  /**
   * Read clipboard using browser Clipboard API (available in Tauri webview)
   */
  async readClipboard(): Promise<string> {
    try {
      return await navigator.clipboard.readText();
    } catch {
      return '';
    }
  }

  /**
   * Write to clipboard using browser Clipboard API (available in Tauri webview)
   */
  async writeClipboard(text: string): Promise<void> {
    await navigator.clipboard.writeText(text);
  }

  async minimizeWindow(): Promise<void> {
    await getCurrentWindow().minimize();
  }

  async toggleMaximizeWindow(): Promise<void> {
    await getCurrentWindow().toggleMaximize();
  }

  async closeWindow(): Promise<void> {
    await getCurrentWindow().close();
  }
}
