import {createAdapter} from '@/platform/adapters'

/**
 * Write text to the clipboard via the platform adapter (Tauri or Web).
 * Use this instead of calling `navigator.clipboard.writeText` directly.
 */
export async function writeClipboard(text: string): Promise<void> {
  await createAdapter().writeClipboard(text)
}
