/**
 * The shortest run of word characters an all-flows search needs (QUERY_TOO_SHORT without one): the server's trigram
 * index draws nothing from a term without one, "---" or "a b" no more than "ab".
 */
export const MIN_SEARCH_TERM_RUN = 3

/**
 * A run of word characters as the server counts them (interfaces.SearchWordChar): a letter, a letter number, a mark
 * Unicode calls alphabetic (the vowel signs inside Devanagari, Tamil, Thai or Arabic words) or a decimal digit of any
 * script. The browser's Unicode tables may differ from the server's, and the server's database may count fewer, so
 * this is a hint: the server's QUERY_TOO_SHORT decides.
 */
const TERM_RUN = new RegExp(`[\\p{Alphabetic}\\p{Nd}]{${MIN_SEARCH_TERM_RUN}}`, 'u')

/** A non-blank term without a run of MIN_SEARCH_TERM_RUN word characters. */
export function shortSearchTerm(q: string): boolean {
  const term = q.trim()
  return term !== '' && !TERM_RUN.test(term)
}
