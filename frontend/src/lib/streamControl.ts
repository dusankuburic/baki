import {isReplicaUnreachable} from '@/lib/chatErrors'

/** A request a rate limit refused (429): nothing it asked for was done, so it may be sent again after Retry-After. */
export function isRateLimited(err: unknown): boolean {
  return typeof err === 'object' && err !== null && (err as {status?: unknown}).status === 429
}

/**
 * A stop or a decision that did not get through and is worth offering again (R-streaming-c20): the server running
 * the stream could not be reached, or a rate limit refused it. Either way nothing was stopped or decided.
 */
export function isStreamControlRetryable(err: unknown): boolean {
  return isReplicaUnreachable(err) || isRateLimited(err)
}
