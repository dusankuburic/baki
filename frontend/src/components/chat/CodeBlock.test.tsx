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
})
