import {describe, it, expect, vi, beforeEach, afterEach} from 'vitest'
import {WebAdapter} from './WebAdapter'

describe('WebAdapter', () => {
  let adapter: WebAdapter

  beforeEach(() => {
    adapter = new WebAdapter()
  })

  afterEach(() => {
    vi.restoreAllMocks()
    // Clean up any stray input elements in body
    const inputs = document.querySelectorAll('input[type="file"]')
    inputs.forEach(i => i.remove())
  })

  describe('fileOpen', () => {
    it('resolves file data on change and cleans up input', async () => {
      const promise = adapter.fileOpen({filters: [{name: 'Text', extensions: ['txt']}]})

      const input = document.querySelector('input[type="file"]') as HTMLInputElement
      expect(input).not.toBeNull()
      expect(input.accept).toBe('.txt')

      const fakeFile = new File(['SET x TO 1\n'], 'Main.txt', {type: 'text/plain'})
      Object.defineProperty(input, 'files', {value: [fakeFile]})

      input.onchange?.({target: input} as unknown as Event)

      const result = await promise
      expect(result).not.toBeNull()
      const parsed = JSON.parse(result as string)
      expect(parsed.__is_web_upload__).toBe(true)
      expect(parsed.name).toBe('Main.txt')
      expect(parsed.files['Main.txt']).toBe('SET x TO 1\n')
      expect(document.querySelector('input[type="file"]')).toBeNull()
    })

    it('resolves null on cancel', async () => {
      const promise = adapter.fileOpen({})
      const input = document.querySelector('input[type="file"]') as HTMLInputElement
      expect(input).not.toBeNull()

      input.oncancel?.(new Event('cancel'))

      const result = await promise
      expect(result).toBeNull()
      expect(document.querySelector('input[type="file"]')).toBeNull()
    })

    it('does not abort when window receives focus', async () => {
      vi.useFakeTimers()
      try {
        const promise = adapter.fileOpen({})
        const input = document.querySelector('input[type="file"]') as HTMLInputElement
        expect(input).not.toBeNull()

        // Simulate focus event on window when returning from OS picker
        window.dispatchEvent(new Event('focus'))
        await vi.advanceTimersByTimeAsync(200)

        // Promise should still be pending
        let resolved = false
        void promise.then(() => {
          resolved = true
        })
        expect(resolved).toBe(false)

        // Now user selects file
        const fakeFile = new File(['content'], 'test.txt', {type: 'text/plain'})
        Object.defineProperty(input, 'files', {value: [fakeFile]})
        input.onchange?.({target: input} as unknown as Event)

        await vi.advanceTimersByTimeAsync(10)
        const result = await promise
        expect(result).not.toBeNull()
      } finally {
        vi.useRealTimers()
      }
    })
  })

  describe('fileOpenDirectory', () => {
    it('reads .txt and .pad files, preserves relative paths, and sets folder name', async () => {
      const promise = adapter.fileOpenDirectory()

      const input = document.querySelector('input[type="file"]') as HTMLInputElement
      expect(input).not.toBeNull()
      expect(input.webkitdirectory).toBe(true)

      const file1 = new File(['Main flow code'], 'Main.txt', {type: 'text/plain'})
      Object.defineProperty(file1, 'webkitRelativePath', {value: 'MyFolder/Main.txt'})

      const file2 = new File(['Sub flow code'], 'Helper.pad', {type: 'text/plain'})
      Object.defineProperty(file2, 'webkitRelativePath', {value: 'MyFolder/Sub/Helper.pad'})

      const ignoredFile = new File(['ignore me'], 'notes.pdf', {type: 'application/pdf'})
      Object.defineProperty(ignoredFile, 'webkitRelativePath', {value: 'MyFolder/notes.pdf'})

      Object.defineProperty(input, 'files', {value: [file1, file2, ignoredFile]})

      input.onchange?.({target: input} as unknown as Event)

      const result = await promise
      expect(result).not.toBeNull()
      const parsed = JSON.parse(result as string)
      expect(parsed.__is_web_upload__).toBe(true)
      expect(parsed.name).toBe('MyFolder')
      expect(parsed.files['Main.txt']).toBe('Main flow code')
      expect(parsed.files['Sub/Helper.pad']).toBe('Sub flow code')
      expect(parsed.files['notes.pdf']).toBeUndefined()
      expect(document.querySelector('input[type="file"]')).toBeNull()
    })

    it('rejects if folder contains no .txt or .pad files', async () => {
      const promise = adapter.fileOpenDirectory()
      const input = document.querySelector('input[type="file"]') as HTMLInputElement

      const file = new File(['binary'], 'image.png', {type: 'image/png'})
      Object.defineProperty(file, 'webkitRelativePath', {value: 'MyFolder/image.png'})
      Object.defineProperty(input, 'files', {value: [file]})

      input.onchange?.({target: input} as unknown as Event)

      await expect(promise).rejects.toThrow(/No \.txt or \.pad flow files/i)
      expect(document.querySelector('input[type="file"]')).toBeNull()
    })

    it('resolves null on cancel without throwing', async () => {
      const promise = adapter.fileOpenDirectory()
      const input = document.querySelector('input[type="file"]') as HTMLInputElement

      input.oncancel?.(new Event('cancel'))

      const result = await promise
      expect(result).toBeNull()
      expect(document.querySelector('input[type="file"]')).toBeNull()
    })

    it('does not abort when window receives focus during folder confirmation prompt', async () => {
      vi.useFakeTimers()
      try {
        const promise = adapter.fileOpenDirectory()
        const input = document.querySelector('input[type="file"]') as HTMLInputElement
        expect(input).not.toBeNull()

        // Simulate window focus firing when browser shows "Upload X files to this site?" prompt
        window.dispatchEvent(new Event('focus'))
        await vi.advanceTimersByTimeAsync(500)

        let settled = false
        promise.then(() => {
          settled = true
        }).catch(() => {
          settled = true
        })
        expect(settled).toBe(false)

        // User now clicks "Upload" in browser prompt
        const file = new File(['Main flow'], 'Main.txt', {type: 'text/plain'})
        Object.defineProperty(file, 'webkitRelativePath', {value: 'FlowFolder/Main.txt'})
        Object.defineProperty(input, 'files', {value: [file]})

        input.onchange?.({target: input} as unknown as Event)

        await vi.advanceTimersByTimeAsync(10)
        const result = await promise
        expect(result).not.toBeNull()
        const parsed = JSON.parse(result as string)
        expect(parsed.name).toBe('FlowFolder')
        expect(parsed.files['Main.txt']).toBe('Main flow')
      } finally {
        vi.useRealTimers()
      }
    })
  })
})
