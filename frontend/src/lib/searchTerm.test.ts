import {describe, it, expect} from 'vitest'
import {shortSearchTerm} from './searchTerm'
// The table the server's rule is tested on too (internal/storage/interfaces/search_term_test.go).
import cases from './searchTerm.cases.json'

describe('shortSearchTerm', () => {
  it('has the whole shared table', () => {
    expect(cases.length).toBeGreaterThanOrEqual(50)
  })

  // A word character is what the server counts: letters with their alphabetic vowel signs, letter numbers and
  // decimal digits of any script. Punctuation, spaces, underscores, symbols and other combining marks split words.
  it.each(cases)('$term is short: $short ($why)', ({term, short}) => {
    expect(shortSearchTerm(term)).toBe(short)
  })
})
