import {AVATAR_IMAGE_MAX_DIMENSION} from './constraints'

/**
 * Downscales an image file to at most maxDimension (default the avatar cap) on its longest side, re-encoding it
 * on the way. The server re-encodes and caps anyway (internal/service/avatar.go); doing it here keeps the upload
 * small — a 12-megapixel photo becomes a ~512 px PNG/JPEG before it ever leaves the device.
 */
export async function downscaleImage(file: File | Blob, maxDimension = AVATAR_IMAGE_MAX_DIMENSION): Promise<Blob> {
  const bitmap = await createImageBitmap(file)
  try {
    const scale = Math.min(1, maxDimension / Math.max(bitmap.width, bitmap.height))
    const width = Math.max(1, Math.round(bitmap.width * scale))
    const height = Math.max(1, Math.round(bitmap.height * scale))
    const canvas = document.createElement('canvas')
    canvas.width = width
    canvas.height = height
    const ctx = canvas.getContext('2d')
    if (!ctx) return file
    ctx.drawImage(bitmap, 0, 0, width, height)
    const type = file.type === 'image/jpeg' ? 'image/jpeg' : 'image/png'
    const blob = await new Promise<Blob | null>(resolve => canvas.toBlob(resolve, type, 0.9))
    return blob ?? file
  } finally {
    bitmap.close()
  }
}
