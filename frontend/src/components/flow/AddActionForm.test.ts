import {describe, it, expect} from 'vitest'
import {composeActionLine, insertBeforeLastRegionEnd} from './AddActionForm'

describe('composeActionLine', () => {
  it('composes type + params + output', () => {
    expect(composeActionLine('Display.ShowMessageBox', "Message: $'''Hi'''", 'Pressed')).toBe(
      "Display.ShowMessageBox Message: $'''Hi''' => Pressed",
    )
  })
  it('omits empty parts', () => {
    expect(composeActionLine('HTTPClient.InvokeUrl', 'Url: https://x', '')).toBe('HTTPClient.InvokeUrl Url: https://x')
    expect(composeActionLine('Comment.Message', '', '')).toBe('Comment.Message')
  })
})

describe('insertBeforeLastRegionEnd', () => {
  it('inserts before the LAST #EndRegion (multi-region source)', () => {
    const src = '#Region "Main"\n    SET A TO 1\n#EndRegion\n#Region "Util"\n    COMMENT  x\n#EndRegion\n'
    const out = insertBeforeLastRegionEnd(src, 'Display.ShowMessageBox Message: Hi')
    // The line lands at the end of the LAST region (Util), indented 4.
    expect(out).toContain('    COMMENT  x\n    Display.ShowMessageBox Message: Hi\n#EndRegion')
    // The FIRST region end marker is untouched.
    expect(out.indexOf('#EndRegion')).toBe(src.indexOf('#EndRegion'))
  })
  it('appends at the end when no region marker exists', () => {
    const out = insertBeforeLastRegionEnd('SET A TO 1\n', 'Comment.Message x')
    expect(out).toBe('SET A TO 1\n    Comment.Message x\n')
  })
  it('matches the marker case-insensitively and trims whitespace', () => {
    const src = '#Region "Main"\n  SET A TO 1\n  #endregion   \n'
    const out = insertBeforeLastRegionEnd(src, 'LABEL Done')
    // The marker line itself is preserved verbatim; the insert precedes it.
    expect(out).toContain('#endregion')
    expect(out.indexOf('    LABEL Done')).toBeLessThan(out.indexOf('#endregion'))
    expect(out.split('\n')).toContain('    LABEL Done')
  })
})
