import {createAdapter} from '@/platform/adapters'

export async function writeClipboard(text: string): Promise<void> {
  await createAdapter().writeClipboard(text)
}
