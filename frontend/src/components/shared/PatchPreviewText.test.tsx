import {describe, it, expect} from 'vitest'
import {render, screen} from '@testing-library/react'
import PatchPreviewText from './PatchPreviewText'

const sample = `Fix "wrap-error-handler" for rule "unhandled-error" on block b1 (HTTPClient.InvokeUrl, line 3):
  insert 2 line(s) before line 4:
    + ON BLOCK ERROR
    +   TRY
  remove lines 5-6
  on line 7 replace 'http://x' with '%Url%'`

// U4.1: the server's scrubbed patch preview renders as a color-coded diff.
describe('PatchPreviewText', () => {
  it('marks + lines as added and remove/replace ops distinctly', () => {
    render(<PatchPreviewText text={sample} />)
    const rows = screen.getByTestId('patch-preview').children
    const kinds = Array.from(rows).map(r => r.className)
    // First line = header; "+ ON BLOCK ERROR" and "+   TRY" = added;
    // "remove lines 5-6" = removed; replace op line = header.
    expect(kinds[0]).toContain('text-text-tertiary')
    expect(kinds[2]).toContain('text-semantic-success')
    expect(kinds[3]).toContain('text-semantic-success')
    const removedIdx = kinds.findIndex(k => k.includes('text-semantic-error'))
    expect(removedIdx).toBeGreaterThan(0)
    expect(rows[removedIdx].textContent).toContain('remove lines 5-6')
  })

  it('strips the leading + marker from added lines (gutter renders it)', () => {
    render(<PatchPreviewText text={'  insert 1 line(s) before line 4:\n    + ON BLOCK ERROR'} />)
    const rows = screen.getByTestId('patch-preview').children
    // The first span is the rendered gutter; the TEXT span must not repeat +.
    const textSpan = rows[1].lastElementChild as HTMLElement
    expect(textSpan.textContent).not.toMatch(/^\+/)
    expect(textSpan.textContent).toContain('ON BLOCK ERROR')
  })
})
