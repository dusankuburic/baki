// uuid() is a secure-context-safe replacement for crypto.randomUUID().
//
// crypto.randomUUID is ONLY defined in a secure context (HTTPS or localhost).
// The chat surface generates ids for every thread, message and stream, so on a
// plain-HTTP deployment — which docker-compose.yml makes an entirely ordinary way
// to run this on a LAN — each of those call sites threw
// `TypeError: crypto.randomUUID is not a function` and chat was unusable.
//
// crypto.getRandomValues has no secure-context requirement, so the fallback is
// still cryptographically random; only the convenience wrapper is missing.
// Math.random is deliberately NOT used — ids leak into stream/thread keys.
export function uuid(): string {
  const c = globalThis.crypto
  if (typeof c?.randomUUID === 'function') return c.randomUUID()

  const bytes = new Uint8Array(16)
  c.getRandomValues(bytes)
  // RFC 4122 §4.4: version 4 in the high nibble of byte 6, variant 10x in byte 8.
  bytes[6] = (bytes[6] & 0x0f) | 0x40
  bytes[8] = (bytes[8] & 0x3f) | 0x80

  const hex: string[] = []
  for (let i = 0; i < 256; i++) hex.push((i + 0x100).toString(16).slice(1))
  const b = Array.from(bytes, byte => hex[byte])
  return `${b[0]}${b[1]}${b[2]}${b[3]}-${b[4]}${b[5]}-${b[6]}${b[7]}-${b[8]}${b[9]}-${b[10]}${b[11]}${b[12]}${b[13]}${b[14]}${b[15]}`
}
