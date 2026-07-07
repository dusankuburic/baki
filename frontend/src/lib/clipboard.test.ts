import {describe, it, expect, vi} from 'vitest'

const writeClipboardMock = vi.fn().mockResolvedValue(undefined)
vi.mock('@/platform/adapters', () => ({
  createAdapter: () => ({writeClipboard: writeClipboardMock}),
}))

describe('writeClipboard', () => {
  it('delegates to the platform adapter writeClipboard method', async () => {
    const {writeClipboard} = await import('./clipboard')
    await writeClipboard('copy me')
    expect(writeClipboardMock).toHaveBeenCalledWith('copy me')
  })
})
