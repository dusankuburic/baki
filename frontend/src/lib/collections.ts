// Immutable Set/Map helpers that produce new instances for React/zustand
// change-detection, eliminating the copy-mutate-replace boilerplate.

export function toggleSetMember<T>(set: Set<T>, value: T): Set<T> {
  const next = new Set(set)
  if (next.has(value)) next.delete(value)
  else next.add(value)
  return next
}

export function mapSet<K, V>(map: Map<K, V>, key: K, value: V): Map<K, V> {
  const next = new Map(map)
  next.set(key, value)
  return next
}

export function mapDelete<K, V>(map: Map<K, V>, key: K): Map<K, V> {
  const next = new Map(map)
  next.delete(key)
  return next
}

export function mapUpdate<K, V>(map: Map<K, V>, key: K, updater: (prev: V | undefined) => V): Map<K, V> {
  const next = new Map(map)
  next.set(key, updater(next.get(key)))
  return next
}
