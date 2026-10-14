import {describe, it, expect, vi, beforeEach, afterEach} from 'vitest'
import {downscaleImage} from './imageResize'

const close = vi.fn()
let drawImage: ReturnType<typeof vi.fn>
let canvasSize: {width: number; height: number}

beforeEach(() => {
  close.mockClear()
  drawImage = vi.fn()
  canvasSize = {width: 0, height: 0}
  vi.stubGlobal('createImageBitmap', vi.fn())
  vi.spyOn(HTMLCanvasElement.prototype, 'getContext').mockImplementation(function (this: HTMLCanvasElement) {
    canvasSize = {width: this.width, height: this.height}
    return {drawImage} as unknown as CanvasRenderingContext2D
  } as never)
  vi.spyOn(HTMLCanvasElement.prototype, 'toBlob').mockImplementation((cb, type) => cb(new Blob(['out'], {type})))
})

afterEach(() => {
  vi.unstubAllGlobals()
  vi.restoreAllMocks()
})

function bitmap(width: number, height: number) {
  vi.mocked(createImageBitmap).mockResolvedValue({width, height, close} as unknown as ImageBitmap)
}

describe('downscaleImage', () => {
  it('scales the longest side down to the cap, keeping the aspect ratio', async () => {
    bitmap(4000, 3000)
    const out = await downscaleImage(new Blob(['in'], {type: 'image/jpeg'}))
    expect(canvasSize).toEqual({width: 512, height: 384})
    expect(drawImage).toHaveBeenCalledWith(expect.anything(), 0, 0, 512, 384)
    expect(out.type).toBe('image/jpeg')
    expect(close).toHaveBeenCalled()
  })

  it('never upscales a small image, and re-encodes anything that is not a JPEG as PNG', async () => {
    bitmap(64, 32)
    const out = await downscaleImage(new Blob(['in'], {type: 'image/webp'}))
    expect(canvasSize).toEqual({width: 64, height: 32})
    expect(out.type).toBe('image/png')
  })

  it('releases the bitmap even when drawing fails', async () => {
    bitmap(100, 100)
    drawImage.mockImplementation(() => {
      throw new Error('boom')
    })
    await expect(downscaleImage(new Blob(['in']))).rejects.toThrow('boom')
    expect(close).toHaveBeenCalled()
  })
})
