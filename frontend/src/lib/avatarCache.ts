import {requestBlob} from '@/api/client'

// Uploaded avatars live behind an authenticated route (/api/users/{id}/avatar): an <img> tag cannot carry the
// Authorization header, so the bytes are fetched with the API client and rendered from an object URL. This module
// is that indirection: one object URL per avatar path, shared by every component that shows it, invalidated by an
// upload or removal (the profile page knows the path it just changed).

// An entry is one fetched image (or the knowledge that the user has none). It goes stale after AVATAR_TTL_MS, so a
// re-upload from another device or user shows up on the next mount without a reload, and the map is capped so a
// long session over a large user list cannot hold unbounded object URLs.
const AVATAR_TTL_MS = 5 * 60 * 1000
const AVATAR_CACHE_MAX = 200

interface Entry {
  url: string // '' = the user has no uploaded image (a 404)
  at: number
}

const cache = new Map<string, Entry>() // path -> entry, least recently set first
const inflight = new Map<string, Promise<string | null>>()
const listeners = new Set<() => void>()

function notify(): void {
  for (const l of listeners) l()
}

function revoke(entry: Entry | undefined): void {
  if (entry?.url) URL.revokeObjectURL(entry.url)
}

function store(path: string, entry: Entry): void {
  cache.delete(path)
  cache.set(path, entry)
  while (cache.size > AVATAR_CACHE_MAX) {
    const oldest = cache.keys().next().value as string
    revoke(cache.get(oldest))
    cache.delete(oldest)
  }
}

/** Drops the cached image for path (after an upload or removal), revoking its object URL. */
export function invalidateAvatar(path: string): void {
  revoke(cache.get(path))
  cache.delete(path)
  notify()
}

export function clearAvatarCache(): void {
  for (const entry of cache.values()) revoke(entry)
  cache.clear()
  notify()
}

/** The avatar path the backend stores in users.avatar_url after an upload (internal/api/auth_avatar.go). */
export function isInternalAvatarUrl(url: string | undefined): boolean {
  return !!url && url.startsWith('/api/users/') && url.endsWith('/avatar')
}

/** Resolves an avatarUrl for rendering: internal paths through the cache, external URLs untouched, null when none. */
export function resolveAvatarUrl(avatarUrl: string | undefined): string | null {
  if (!avatarUrl) return null
  if (!isInternalAvatarUrl(avatarUrl)) return avatarUrl
  return cache.get(avatarUrl)?.url || null
}

function isNotFound(err: unknown): boolean {
  return typeof err === 'object' && err !== null && (err as {status?: unknown}).status === 404
}

/**
 * Loads the avatar at path into the cache and returns its object URL, or null when the user has none.
 * Concurrent callers share one request. Only a 404 is remembered as "no image"; any other failure (offline, 5xx)
 * is not cached, so the next render retries, and a stale image keeps showing meanwhile.
 */
export function loadAvatar(path: string): Promise<string | null> {
  const cached = cache.get(path)
  if (cached && Date.now() - cached.at < AVATAR_TTL_MS) return Promise.resolve(cached.url || null)
  const pending = inflight.get(path)
  if (pending) return pending
  const promise = (async () => {
    try {
      const url = URL.createObjectURL(await requestBlob(path))
      const old = cache.get(path)
      store(path, {url, at: Date.now()})
      notify()
      revoke(old) // after the listeners switched to the new URL
      return url
    } catch (err) {
      if (isNotFound(err)) {
        const old = cache.get(path)
        store(path, {url: '', at: Date.now()})
        notify()
        revoke(old)
        return null
      }
      return cached?.url || null
    }
  })().finally(() => {
    if (inflight.get(path) === promise) inflight.delete(path)
  })
  inflight.set(path, promise)
  return promise
}

/** Runs fn whenever a cached avatar changes (loaded, invalidated). Returns an unsubscribe. */
export function onAvatarsChanged(fn: () => void): () => void {
  listeners.add(fn)
  return () => {
    listeners.delete(fn)
  }
}

export function avatarCacheSize(): number {
  return cache.size
}
