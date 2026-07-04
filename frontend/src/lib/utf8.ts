// utf8ByteLength returns the UTF-8 byte length of s. The chat delta-resume
// path sends this as the `from` offset to the backend, which slices its Go
// string buffer by BYTE index. JS string .length counts UTF-16 code units,
// which mismatches bytes for any non-ASCII content (emoji/CJK/accented Latin)
// and would corrupt the resumed tail — hence this helper.
const encoder = typeof TextEncoder !== 'undefined' ? new TextEncoder() : null

export function utf8ByteLength(s: string): number {
  if (encoder) return encoder.encode(s).length
  return s.length
}
