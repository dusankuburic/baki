export function describeUserAgent(ua: string | undefined): string {
  if (!ua) return 'Unknown device'

  const browser = /Edg\//.test(ua)
    ? 'Edge'
    : /OPR\//.test(ua)
      ? 'Opera'
      : /Firefox\//.test(ua)
        ? 'Firefox'
        : /CriOS\//.test(ua)
          ? 'Chrome'
          : /Chrome\//.test(ua)
            ? 'Chrome'
            : /Safari\//.test(ua) && /Version\//.test(ua)
              ? 'Safari'
              : null

  const os = /Windows/.test(ua)
    ? 'Windows'
    : /iPhone|iPad|iPod/.test(ua)
      ? 'iOS'
      : /Mac OS X|Macintosh/.test(ua)
        ? 'macOS'
        : /Android/.test(ua)
          ? 'Android'
          : /Linux/.test(ua)
            ? 'Linux'
            : null

  if (browser && os) return `${browser} on ${os}`
  if (browser) return browser
  if (os) return os
  return 'Unknown device'
}

export type DeviceKind = 'desktop' | 'mobile' | 'tablet' | 'unknown'

export function deviceKind(ua: string | undefined): DeviceKind {
  if (!ua) return 'unknown'
  if (/iPad|Tablet/i.test(ua) || (/Android/.test(ua) && !/Mobile/.test(ua))) return 'tablet'
  if (/iPhone|iPod|Mobile/i.test(ua)) return 'mobile'
  if (/Windows|Macintosh|Mac OS X|Linux|CrOS|X11/.test(ua)) return 'desktop'
  return 'unknown'
}
