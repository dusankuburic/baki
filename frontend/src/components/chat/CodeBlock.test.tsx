import {describe, it, expect, vi, beforeEach, afterEach} from 'vitest'
import {render, screen, fireEvent} from '@testing-library/react'
import CodeBlock from './CodeBlock'

// Mock clipboard to avoid jsdom limitations
vi.mock('@/lib/clipboard', () => ({
  writeClipboard: vi.fn().mockResolvedValue(undefined),
}))

import {writeClipboard} from '@/lib/clipboard'

describe('CodeBlock', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    vi.useFakeTimers()
  })

  afterEach(() => {
    vi.useRealTimers()
  })

  it('renders the code content', () => {
    const {container} = render(<CodeBlock language="typescript" value="const x = 42" />)
    // prism-react-renderer tokenizes code into spans, so check the <pre> text content
    const pre = container.querySelector('pre')
    expect(pre?.textContent).toContain('const')
    expect(pre?.textContent).toContain('42')
  })

  it('displays the language label', () => {
    render(<CodeBlock language="python" value="print('hello')" />)
    expect(screen.getByText('python')).toBeTruthy()
  })

  it('defaults to "text" when no language is provided', () => {
    render(<CodeBlock value="plain text" />)
    expect(screen.getByText('text')).toBeTruthy()
  })

  it('strips trailing newline from display', () => {
    render(<CodeBlock value="line1\n" />)
    // The component strips the trailing \n; the pre element should not show it
    const pre = document.querySelector('pre')
    expect(pre).toBeTruthy()
    expect(pre?.textContent).not.toContain('\n\n')
  })

  it('copies code to clipboard on button click', () => {
    const code = 'console.log("test")'
    render(<CodeBlock language="javascript" value={code} />)

    const copyButton = screen.getByTitle('Copy code')
    fireEvent.click(copyButton)

    expect(writeClipboard).toHaveBeenCalledWith(code)
  })

  it('shows "Copied" feedback after clicking copy', async () => {
    vi.useRealTimers()
    render(<CodeBlock language="go" value='fmt.Println("hi")' />)

    fireEvent.click(screen.getByTitle('Copy code'))

    // The copied state is set synchronously after writeClipboard resolves
    await screen.findByText('Copied')
    expect(screen.getByText('Copied')).toBeTruthy()
  })

  // Prism names its token classes after the grammar ("token table"), and
  // Tailwind ships a `table` utility (`display: table`): a markdown table in
  // a code fence rendered one cell per line.
  it('keeps Prism token classes out of the Tailwind utility namespace', () => {
    const table = '| a | b |\n| --- | --- |\n| 1 | 2 |'
    const {container} = render(<CodeBlock language="markdown" value={table} />)
    // The grammar did tokenize the table…
    expect(container.querySelector('pre .tok-table')).not.toBeNull()
    // …and no span carries a bare grammar name.
    expect(container.querySelector('pre .table')).toBeNull()
    const spans = Array.from(container.querySelectorAll('pre span'))
    expect(spans.length).toBeGreaterThan(0)
    for (const span of spans) {
      for (const cls of span.className.split(' ')) expect(cls.startsWith('tok-')).toBe(true)
    }
  })

  it('wraps long lines by default; the toggle switches the block to horizontal scrolling', () => {
    const {container} = render(<CodeBlock language="text" value="a very long line" />)
    const pre = container.querySelector('pre')!
    expect(pre.className).toContain('whitespace-pre-wrap')
    const toggle = screen.getByRole('button', {name: 'Wrap long lines'})
    expect(toggle.getAttribute('aria-pressed')).toBe('true')

    fireEvent.click(toggle)
    expect(toggle.getAttribute('aria-pressed')).toBe('false')
    expect(pre.className).not.toContain('whitespace-pre-wrap')
    expect(pre.className).toContain('whitespace-pre')
    expect(pre.className).toContain('overflow-x-auto')
  })

  it('wraps a streaming (plain) block the same way', () => {
    const {container} = render(<CodeBlock language="text" value="streaming" plain />)
    expect(container.querySelector('pre')!.className).toContain('whitespace-pre-wrap')
  })
})
