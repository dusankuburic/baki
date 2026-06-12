/**
 * Web-specific platform adapter implementation
 */

import type {
  PlatformAdapter,
  BackendConfig,
  FileOpenOptions,
  FileSaveOptions,
  NotificationOptions,
} from '../types';
import {logger} from '@/lib/logger';

/**
 * Web adapter for browser-specific operations
 */
export class WebAdapter implements PlatformAdapter {
  private config: BackendConfig | null = null;

  /**
   * Get backend configuration from environment or API
   */
  async getBackendConfig(): Promise<BackendConfig> {
    if (this.config) {
      return this.config;
    }

    const apiUrl = import.meta.env.VITE_API_URL || window.location.origin;

    // In local (non-JWT) mode the backend generates a random per-session token
    // and exposes it on a public endpoint so the web client can self-configure.
    // In JWT/cloud mode this endpoint returns 404 and the client authenticates
    // via login instead (sessionToken is set by authStore after login).
    let token: string | undefined;
    try {
      const res = await fetch(`${apiUrl}/api/local-config`);
      if (res.ok) {
        const data = await res.json();
        token = data.token;
      }
    } catch {
      // backend unreachable or JWT mode — leave token undefined
    }

    this.config = {
      apiUrl,
      token,
      version: import.meta.env.VITE_APP_VERSION,
    };

    return this.config;
  }

  /**
   * Open file dialog using HTML5 File API
   */
  async fileOpen(options: FileOpenOptions): Promise<string | string[] | null> {
    return new Promise((resolve) => {
      // Create hidden file input
      const input = document.createElement('input');
      input.type = 'file';
      input.multiple = options.multiple || false;
      input.style.display = 'none';

      // Apply file filters if specified
      if (options.filters && options.filters.length > 0) {
        const extensions = options.filters.flatMap(f => f.extensions);
        input.accept = extensions.map(ext => `.${ext}`).join(',');
      }

      input.onchange = (e) => {
        const files = (e.target as HTMLInputElement).files;
        if (!files || files.length === 0) {
          resolve(null);
          return;
        }

        const file = files[0];
        const reader = new FileReader();
        reader.onload = (e) => {
          const content = e.target?.result as string;
          resolve(JSON.stringify({
            __is_web_upload__: true,
            name: file.name,
            files: { [file.name]: content }
          }));
        };
        reader.readAsText(file);

        // Clean up
        document.body.removeChild(input);
      };

      input.oncancel = () => {
        document.body.removeChild(input);
        resolve(null);
      };

      // Add to DOM and trigger click
      document.body.appendChild(input);
      input.click();
    });
  }

  /**
   * Open directory dialog using webkitdirectory
   */
  async fileOpenDirectory(): Promise<string | null> {
    return new Promise((resolve) => {
      const input = document.createElement('input');
      input.type = 'file';
      input.webkitdirectory = true;
      input.style.display = 'none';

      input.onchange = async (e) => {
        const files = (e.target as HTMLInputElement).files;
        if (!files || files.length === 0) {
          resolve(null);
          return;
        }

        const fileMap: Record<string, string> = {};
        let directoryName = '';

        const promises = Array.from(files)
          .filter(file => file.name.toLowerCase().endsWith('.txt'))
          .map(file => {
            if (!directoryName && file.webkitRelativePath) {
              directoryName = file.webkitRelativePath.split('/')[0];
            }
            return new Promise<void>((res) => {
              const reader = new FileReader();
              reader.onload = (e) => {
                fileMap[file.name] = e.target?.result as string;
                res();
              };
              reader.readAsText(file);
            });
          });

        await Promise.all(promises);
        
        if (Object.keys(fileMap).length === 0) {
          resolve(null);
          return;
        }

        // Return a special JSON string that flowApi will recognize
        resolve(JSON.stringify({
          __is_web_upload__: true,
          name: directoryName || 'Uploaded Folder',
          files: fileMap
        }));

        document.body.removeChild(input);
      };

      input.oncancel = () => {
        document.body.removeChild(input);
        resolve(null);
      };

      document.body.appendChild(input);
      input.click();
    });
  }

  /**
   * Save file using browser download
   */
  async fileSave(_options: FileSaveOptions): Promise<string | null> {
    logger.warn('File save dialogs not supported in web browsers. Use download instead.');
    return null;
  }

  /**
   * Reveal file - not applicable in web browsers
   */
  async fileReveal(_path: string): Promise<void> {
    // Revealing a file in the OS file manager has no browser equivalent — no-op.
    logger.warn('File reveal is not supported in web browsers');
  }

  /**
   * Open URL in new tab
   */
  async openURL(url: string): Promise<void> {
    window.open(url, '_blank');
  }

  /**
   * Show browser notification
   */
  async showNotification(options: NotificationOptions): Promise<void> {
    if ('Notification' in window) {
      if (Notification.permission === 'granted') {
        new Notification(options.title, {
          body: options.body,
          icon: options.icon,
        });
      } else if (Notification.permission !== 'denied') {
        Notification.requestPermission().then((permission) => {
          if (permission === 'granted') {
            new Notification(options.title, {
              body: options.body,
              icon: options.icon,
            });
          }
        });
      }
    } else {
      logger.warn('Notifications not supported in this browser');
    }
  }

  /**
   * Read clipboard using Clipboard API
   */
  async readClipboard(): Promise<string> {
    try {
      if (navigator.clipboard && navigator.clipboard.readText) {
        return await navigator.clipboard.readText();
      } else {
        logger.warn('Clipboard API not available');
        return '';
      }
    } catch (error) {
      console.error('Failed to read clipboard:', error);
      return '';
    }
  }

  /**
   * Write to clipboard using Clipboard API
   */
  async writeClipboard(text: string): Promise<void> {
    try {
      if (navigator.clipboard && navigator.clipboard.writeText) {
        await navigator.clipboard.writeText(text);
      } else {
        logger.warn('Clipboard API not available');
        throw new Error('Clipboard API not available');
      }
    } catch (error) {
      console.error('Failed to write to clipboard:', error);
      throw error;
    }
  }

  // Window controls are managed by the browser chrome in web mode — no-ops.
  async minimizeWindow(): Promise<void> {}
  async toggleMaximizeWindow(): Promise<void> {}
  async closeWindow(): Promise<void> {}
}