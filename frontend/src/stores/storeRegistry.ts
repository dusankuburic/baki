
import {logger} from '@/lib/logger'
import {clearRequestCache, abortAllRequests} from '@/api/client'

type ResetHandler = () => void | Promise<void>

const handlers = new Set<ResetHandler>()

export function registerStoreReset(handler: ResetHandler): void {
  handlers.add(handler)
}

let userIdResolver: () => string | null = () => null

export function registerCurrentUserResolver(resolver: () => string | null): void {
  userIdResolver = resolver
}

export function currentUserId(): string | null {
  return userIdResolver()
}

export async function resetAllStores(): Promise<void> {
  clearRequestCache()
  abortAllRequests()
  const run = async (fn: ResetHandler, i: number) => {
    try {
      await fn()
    } catch (err) {
      logger.error('store reset handler failed during logout (index', i, '):', err)
    }
  }
  await Promise.all([...handlers].map((fn, i) => run(fn, i)))
}
