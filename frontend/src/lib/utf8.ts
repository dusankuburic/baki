const encoder = typeof TextEncoder !== 'undefined' ? new TextEncoder() : null

export function utf8ByteLength(s: string): number {
  if (encoder) return encoder.encode(s).length
  return s.length
}
