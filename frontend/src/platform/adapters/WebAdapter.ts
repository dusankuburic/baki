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

/**
 * Web adapter for browser-specific operations
 */
export class WebAdapter implements PlatformAdapter {
  private config: BackendConfig | null = null;

  /**
   * Get backend configuration from environment or API
   */
  async getBackendConfig(): Promise<BackendConfig> {
    // Return cached config if available
    if (this.config) {
      return this.config;
    }

    // For web deployment, get configuration from environment variables
    const apiUrl = import.meta.env.VITE_API_URL || window.location.origin + '/api';
    const token = undefined;

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

        // For web, we return file content instead of paths
        if (options.multiple) {
          const fileContents: string[] = [];
          Array.from(files).forEach(file => {
            const reader = new FileReader();
            reader.onload = (e) => {
              fileContents.push(e.target?.result as string);
              if (fileContents.length === files.length) {
                resolve(fileContents);
              }
            };
            reader.readAsText(file);
          });
        } else {
          const reader = new FileReader();
          reader.onload = (e) => {
            resolve(e.target?.result as string || null);
          };
          reader.readAsText(files[0]);
        }

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
   * Directory open not supported in web browsers
   */
  async fileOpenDirectory(): Promise<string | null> {
    console.warn('Directory selection not supported in web browsers');
    return null;
  }

  /**
   * Save file using browser download
   */
  async fileSave(_options: FileSaveOptions): Promise<string | null> {
    console.warn('File save dialogs not supported in web browsers. Use download instead.');
    return null;
  }

  /**
   * Reveal file - not applicable in web browsers
   */
  async fileReveal(_path: string): Promise<void> {
    throw new Error('File reveal not supported in web browsers');
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
      console.warn('Notifications not supported in this browser');
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
        console.warn('Clipboard API not available');
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
        console.warn('Clipboard API not available');
        throw new Error('Clipboard API not available');
      }
    } catch (error) {
      console.error('Failed to write to clipboard:', error);
      throw error;
    }
  }
}