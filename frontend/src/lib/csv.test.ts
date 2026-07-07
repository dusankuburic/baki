import {describe, it, expect, vi, afterEach} from 'vitest'
import {csvCell, downloadBlob} from './csv'

describe('csvCell', () => {
  it('returns simple values unquoted', () => {
    expect(csvCell('hello')).toBe('hello')
  })

  it('quotes values containing a comma', () => {
    expect(csvCell('a,b')).toBe('"a,b"')
  })

  it('quotes and escapes embedded double quotes', () => {
    expect(csvCell('say "hi"')).toBe('"say ""hi"""')
  })

  it('quotes values containing a newline', () => {
    expect(csvCell('line1\nline2')).toBe('"line1\nline2"')
  })

  it('neutralizes formula-injection prefixes with a leading single quote', () => {
    expect(csvCell('=SUM(A1:A9)')).toBe("'=SUM(A1:A9)")
    expect(csvCell('+1234')).toBe("'+1234")
    expect(csvCell('-1234')).toBe("'-1234")
    expect(csvCell('@cmd')).toBe("'@cmd")
  })

  it('quotes a formula-injection value that also needs comma quoting', () => {
    expect(csvCell('=A1,A2')).toBe('"\'=A1,A2"')
  })
})

describe('downloadBlob', () => {
  afterEach(() => {
    vi.restoreAllMocks()
  })

  it('creates an object URL, triggers a click, and revokes the URL', () => {
    const createUrl = vi.spyOn(URL, 'createObjectURL').mockReturnValue('blob:mock-url')
    const revokeUrl = vi.spyOn(URL, 'revokeObjectURL').mockImplementation(() => {})
    const clickSpy = vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(() => {})

    downloadBlob('a,b,c', 'text/csv', 'test.csv')

    expect(createUrl).toHaveBeenCalledTimes(1)
    expect(clickSpy).toHaveBeenCalledTimes(1)
    expect(revokeUrl).toHaveBeenCalledWith('blob:mock-url')
  })
})
