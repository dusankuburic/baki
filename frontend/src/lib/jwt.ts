
export function decodeJwtPayload(token: string): Record<string, unknown> | null {
  const parts = token.split('.')
  if (parts.length !== 3) return null
  try {
    const segment = parts[1]
    if (!segment) return null
    let b64 = segment.replace(/-/g, '+').replace(/_/g, '/')
    switch (b64.length % 4) {
      case 2:
        b64 += '=='
        break
      case 3:
        b64 += '='
        break
      case 1:
        return null
    }
    const parsed: unknown = JSON.parse(atob(b64))
    return parsed && typeof parsed === 'object' ? (parsed as Record<string, unknown>) : null
  } catch {
    return null
  }
}
