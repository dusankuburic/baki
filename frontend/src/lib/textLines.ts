
export function countNewlines(s: string, from = 0, to = s.length): number {
  let n = 0
  let i = s.indexOf('\n', from)
  while (i !== -1 && i < to) {
    n++
    i = s.indexOf('\n', i + 1)
  }
  return n
}

export function countLines(s: string): number {
  return countNewlines(s) + 1
}

const CHUNK = 4096

function commonPrefixLength(a: string, b: string): number {
  const max = Math.min(a.length, b.length)
  let i = 0
  while (i + CHUNK <= max && a.slice(i, i + CHUNK) === b.slice(i, i + CHUNK)) i += CHUNK
  while (i < max && a.charCodeAt(i) === b.charCodeAt(i)) i++
  return i
}

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

export function lineCountAfterEdit(prev: string, next: string, prevLineCount: number): number {
  if (prev === next) return prevLineCount
  const prefix = commonPrefixLength(prev, next)
  const suffix = commonSuffixLength(prev, next, Math.min(prev.length, next.length) - prefix)
  const removed = countNewlines(prev, prefix, prev.length - suffix)
  const added = countNewlines(next, prefix, next.length - suffix)
  return prevLineCount - removed + added
}

export interface LineRange {
  first: number
  end: number
}

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
