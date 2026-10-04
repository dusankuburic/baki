// Line bookkeeping for the source editor's gutter, written so a keystroke in
// a 30k-line buffer touches the changed region, not the whole text: the
// editor used to `split('\n')` the entire source per keystroke and render one
// node per line.

/** Newlines in `s[from, to)`. A native indexOf scan — no per-line allocation. */
export function countNewlines(s: string, from = 0, to = s.length): number {
  let n = 0
  let i = s.indexOf('\n', from)
  while (i !== -1 && i < to) {
    n++
    i = s.indexOf('\n', i + 1)
  }
  return n
}

/** Lines in `s` (an empty string is one line, like a textarea shows it). */
export function countLines(s: string): number {
  return countNewlines(s) + 1
}

// Prefix/suffix matching compares native-sized chunks first (a slice of a
// flat string is a view and `===` a memcmp), then the last chunk by char.
const CHUNK = 4096

function commonPrefixLength(a: string, b: string): number {
  const max = Math.min(a.length, b.length)
  let i = 0
  while (i + CHUNK <= max && a.slice(i, i + CHUNK) === b.slice(i, i + CHUNK)) i += CHUNK
  while (i < max && a.charCodeAt(i) === b.charCodeAt(i)) i++
  return i
}

// Suffix length, never reaching into the first `limit` chars of either string
// (so prefix and suffix cannot overlap when text repeats around the edit).
function commonSuffixLength(a: string, b: string, limit: number): number {
  let i = 0
  while (
    i + CHUNK <= limit &&
    a.slice(a.length - i - CHUNK, a.length - i) === b.slice(b.length - i - CHUNK, b.length - i)
  ) {
    i += CHUNK
  }
  while (i < limit && a.charCodeAt(a.length - 1 - i) === b.charCodeAt(b.length - 1 - i)) i++
  return i
}

/**
 * Line count of `next`, given `prev` and its line count: the common prefix
 * and suffix are skipped and only the replaced region of each string is
 * scanned for newlines.
 */
export function lineCountAfterEdit(prev: string, next: string, prevLineCount: number): number {
  if (prev === next) return prevLineCount
  const prefix = commonPrefixLength(prev, next)
  const suffix = commonSuffixLength(prev, next, Math.min(prev.length, next.length) - prefix)
  const removed = countNewlines(prev, prefix, prev.length - suffix)
  const added = countNewlines(next, prefix, next.length - suffix)
  return prevLineCount - removed + added
}

export interface LineRange {
  /** First line to render (0-based, inclusive). */
  first: number
  /** Line to stop at (0-based, exclusive). */
  end: number
}

/**
 * The lines a gutter must render for a viewport: those intersecting
 * `[scrollTop, scrollTop + viewportHeight)` plus `buffer` lines either side,
 * clamped to the document.
 */
export function visibleLineRange(
  scrollTop: number,
  viewportHeight: number,
  lineHeight: number,
  lineCount: number,
  buffer: number,
): LineRange {
  const first = Math.max(0, Math.floor(scrollTop / lineHeight) - buffer)
  const end = Math.min(lineCount, Math.ceil((scrollTop + viewportHeight) / lineHeight) + buffer)
  return {first, end: Math.max(first, end)}
}
