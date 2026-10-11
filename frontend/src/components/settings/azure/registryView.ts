import {
  CHAT_MODEL_KINDS,
  isKnown,
  type AzureConnection,
  type AzureDiscovery,
  type AzureDiscoveryItem,
  type AzureModelEntry,
  type AzureModelKind,
  type AzureRegistryLimits,
  type AzureScope,
  type Tolerant,
} from '@/types'

export const AZURE_SECTION_ANCHOR = 'provider-azure-foundry'

const ORG_ANCHOR_PREFIX = 'org-azure-foundry-'

export function orgAzureAnchor(orgId: string): string {
  return `${ORG_ANCHOR_PREFIX}${orgId}`
}

export function orgIdFromAnchor(anchor: string | null | undefined): string | null {
  return anchor && anchor.startsWith(ORG_ANCHOR_PREFIX) && anchor.length > ORG_ANCHOR_PREFIX.length
    ? anchor.slice(ORG_ANCHOR_PREFIX.length)
    : null
}

type Owned = {shared?: boolean}

export function isOwnItem(scope: AzureScope, item: Owned): boolean {
  return scope.kind === 'org' || !item.shared
}

export function isChatKind(kind: Tolerant<AzureModelKind>): boolean {
  return isKnown(CHAT_MODEL_KINDS, kind)
}

export interface SharedOrgGroup {
  orgId: string
  orgName: string
  connections: AzureConnection[]
  models: AzureModelEntry[]
}

export function sharedGroups(connections: AzureConnection[], models: AzureModelEntry[]): SharedOrgGroup[] {
  const groups = new Map<string, SharedOrgGroup>()
  const group = (orgId: string, orgName: string | undefined) => {
    let g = groups.get(orgId)
    if (!g) {
      g = {orgId, orgName: orgName || orgId, connections: [], models: []}
      groups.set(orgId, g)
    } else if (orgName && g.orgName === orgId) {
      g.orgName = orgName
    }
    return g
  }
  for (const c of connections) if (c.shared && c.orgId) group(c.orgId, c.orgName).connections.push(c)
  for (const m of models) if (m.shared && m.orgId) group(m.orgId, m.orgName).models.push(m)
  return [...groups.values()]
    .filter(g => g.models.length > 0)
    .sort(
      (a, b) => a.orgName.localeCompare(b.orgName, undefined, {sensitivity: 'base'}) || (a.orgId < b.orgId ? -1 : 1),
    )
}

export interface LimitUse {
  used: number
  limit: number
  reached: boolean
}

export interface LimitsView {
  connections: LimitUse
  models: LimitUse
  show: boolean
}

export function limitsView(connections: number, models: number, limits: AzureRegistryLimits): LimitsView {
  const usage = (used: number, limit: number): LimitUse => ({used, limit, reached: limit > 0 && used >= limit})
  const near = (used: number, limit: number) => limit > 0 && used * 5 >= limit * 4
  return {
    connections: usage(connections, limits.connections),
    models: usage(models, limits.models),
    show: near(connections, limits.connections) || near(models, limits.models),
  }
}

export function effectiveDefaultModelId(
  saved: string | null | undefined,
  serverDefault: string | null | undefined,
  models: readonly Pick<AzureModelEntry, 'modelId'>[],
): string | null {
  const listed = (id: string | null | undefined): id is string => !!id && models.some(m => m.modelId === id)
  if (listed(saved)) return saved
  return listed(serverDefault) ? serverDefault : null
}

function nameSpace(kind: Tolerant<AzureModelKind>): string {
  return kind === 'claude' ? 'chat' : kind
}

export function findDuplicateModel(
  models: readonly AzureModelEntry[],
  draft: {connectionId: string; kind: AzureModelKind; deployment: string},
  selfId?: string,
): AzureModelEntry | undefined {
  const name = draft.deployment.trim().toLowerCase()
  if (!name) return undefined
  return models.find(
    m =>
      m.id !== selfId &&
      m.connectionId === draft.connectionId &&
      nameSpace(m.kind) === nameSpace(draft.kind) &&
      m.deployment.toLowerCase() === name,
  )
}

export function findDisplayNameTwin(
  models: readonly AzureModelEntry[],
  displayName: string,
  selfId?: string,
): AzureModelEntry | undefined {
  const name = displayName.trim().toLowerCase()
  if (!name) return undefined
  return models.find(m => m.id !== selfId && m.displayName.trim().toLowerCase() === name)
}

export function unaddedItems(discovery: AzureDiscovery | null | undefined): AzureDiscoveryItem[] {
  if (!discovery) return []
  return [...discovery.deployments, ...discovery.agents].filter(item => !item.alreadyAdded)
}

export function unaddedCounts(discovery: AzureDiscovery | null | undefined): {deployments: number; agents: number} {
  if (!discovery) return {deployments: 0, agents: 0}
  return {
    deployments: discovery.deployments.filter(d => !d.alreadyAdded).length,
    agents: discovery.agents.filter(a => !a.alreadyAdded).length,
  }
}

export function discoveryItemKey(item: Pick<AzureDiscoveryItem, 'kind' | 'name'>): string {
  return `${item.kind === 'agent' ? 'agent' : 'deployment'}:${item.name.toLowerCase()}`
}

export const focusKey = {
  connection: (id: string) => `connection:${id}`,
  addModel: (connectionId: string) => `add-model:${connectionId}`,
  modelName: (id: string) => `model-name:${id}`,
  modelEdit: (id: string) => `model-edit:${id}`,
  addResource: 'add-resource',
  discoveryToggle: (connectionId: string) => `discovery-toggle:${connectionId}`,
  discoveryAdd: (connectionId: string, key: string) => `discovery-add:${connectionId}:${key}`,
}

export function focusAfterModelRemoval(rows: readonly string[], removedId: string, connectionId: string): string {
  const at = rows.indexOf(removedId)
  const next = at >= 0 ? (rows[at + 1] ?? rows[at - 1]) : undefined
  return next ? focusKey.modelEdit(next) : focusKey.addModel(connectionId)
}

export function focusAfterConnectionRemoval(cards: readonly string[], removedId: string): string {
  const at = cards.indexOf(removedId)
  const next = at >= 0 ? (cards[at + 1] ?? cards[at - 1]) : undefined
  return next ? focusKey.connection(next) : focusKey.addResource
}
