// Minimal, dependency-free JWT payload decoding for client-side expiry checks.
//
// We only ever need to peek at the `exp` claim to decide whether to proactively
// refresh; the token is always verified server-side, so this never makes a trust
// decision on its own.

/**
 * Decode a JWT's payload (the middle segment) into a plain object, or return
 * `null` if the token is structurally malformed or not valid base64url JSON.
 *
 * JWT segments are base64url (RFC 7515 §2): they use `-`/`_` instead of `+`/`/`
 * and drop `=` padding. The browser's `atob()` only accepts standard base64 and
 * throws an `InvalidCharacterError` on `-`/`_`, so we translate the alphabet and
 * restore padding before decoding. (Previously two call sites passed the raw
 * segment straight to `atob`, which threw for any token whose payload base64
 * happened to contain `-`/`_`.)
 */
export function decodeJwtPayload(token: string): Record<string, unknown> | null {
  const parts = token.split('.')
  if (parts.length !== 3) return null // not a JWT (e.g. the local-mode static token)
  try {
    let b64 = parts[1].replace(/-/g, '+').replace(/_/g, '/')
    switch (b64.length % 4) {
      case 2:
        b64 += '=='
        break
      case 3:
        b64 += '='
        break
      case 1:
        return null // not a valid base64 length
    }
    const parsed: unknown = JSON.parse(atob(b64))
    return parsed && typeof parsed === 'object' ? (parsed as Record<string, unknown>) : null
  } catch {
    return null
  }
}
