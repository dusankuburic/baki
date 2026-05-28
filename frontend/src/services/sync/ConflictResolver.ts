import type { BlockPayload } from '@/services/collaboration/CollaborationService'

export type ConflictStrategy = 'last-write-wins' | 'server-wins' | 'client-wins'

export interface ConflictResult<T> {
  resolved: T
  strategy: ConflictStrategy
  hadConflict: boolean
}

export interface VersionedBlock {
  blockId: string
  subflowId: string
  properties: Record<string, unknown>
  version: number
  updatedAt: number
}

/**
 * Resolves a conflict between a local (optimistic) block update and the
 * authoritative server version.
 *
 * The default strategy is last-write-wins based on `updatedAt` timestamps.
 * When timestamps are equal the server version takes precedence.
 */
export function resolveBlockConflict(
  local: VersionedBlock,
  server: VersionedBlock,
  strategy: ConflictStrategy = 'last-write-wins',
): ConflictResult<VersionedBlock> {
  const hadConflict = local.version !== server.version

  if (!hadConflict) {
    return { resolved: server, strategy, hadConflict: false }
  }

  let resolved: VersionedBlock

  switch (strategy) {
    case 'client-wins':
      resolved = { ...local, version: server.version + 1 }
      break
    case 'server-wins':
      resolved = server
      break
    case 'last-write-wins':
    default:
      resolved = local.updatedAt > server.updatedAt
        ? { ...local, version: server.version + 1 }
        : server
      break
  }

  return { resolved, strategy, hadConflict: true }
}

/**
 * Merges two property maps shallowly.
 * Fields present only in `remote` are accepted.
 * Fields present only in `local` (created after last sync) are kept.
 * Fields present in both use the conflict strategy.
 */
export function mergeProperties(
  local: Record<string, unknown>,
  remote: Record<string, unknown>,
  baselineKeys: Set<string>,
  strategy: ConflictStrategy = 'last-write-wins',
): Record<string, unknown> {
  const merged = { ...remote }

  for (const key of Object.keys(local)) {
    if (!baselineKeys.has(key)) {
      // New local key — always keep
      merged[key] = local[key]
    } else if (strategy === 'client-wins') {
      merged[key] = local[key]
    }
    // server-wins and last-write-wins fall through to remote value
  }

  return merged
}

/**
 * Converts a BlockPayload (from a WebSocket envelope) to a VersionedBlock,
 * stamping the current time as updatedAt.
 */
export function blockPayloadToVersioned(p: BlockPayload, updatedAt = Date.now()): VersionedBlock {
  return {
    blockId: p.blockId,
    subflowId: p.subflowId,
    properties: (p.properties ?? {}) as Record<string, unknown>,
    version: p.version,
    updatedAt,
  }
}
