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

// The AI Accounts registry view's pure half: which items a scope owns, the
// "Shared with you" groups, the limits line, the effective default, the
// duplicate rule, discovery counts, and the focus targets the panel moves
// to after an action. No copy: the components translate.

/** The heading of the Azure AI Foundry section in AI Accounts (a Settings deep-link anchor). */
export const AZURE_SECTION_ANCHOR = 'provider-azure-foundry'

const ORG_ANCHOR_PREFIX = 'org-azure-foundry-'

/** The heading of an organisation's Azure AI Foundry block in Organizations ("Manage in Organizations"). */
export function orgAzureAnchor(orgId: string): string {
  return `${ORG_ANCHOR_PREFIX}${orgId}`
}

/** The organisation an orgAzureAnchor names, or null for any other anchor. */
export function orgIdFromAnchor(anchor: string | null | undefined): string | null {
  return anchor && anchor.startsWith(ORG_ANCHOR_PREFIX) && anchor.length > ORG_ANCHOR_PREFIX.length
    ? anchor.slice(ORG_ANCHOR_PREFIX.length)
    : null
}

type Owned = {shared?: boolean}

/**
 * The items a scope's own routes act on: every item in org scope, the
 * caller's personal ones in user scope (the shared entries of their
 * organisations are read-only there).
 */
export function isOwnItem(scope: AzureScope, item: Owned): boolean {
  return scope.kind === 'org' || !item.shared
}

/** Kinds the chat can talk to: an embedding model only embeds, and a kind this bundle does not know is not offered. */
export function isChatKind(kind: Tolerant<AzureModelKind>): boolean {
  return isKnown(CHAT_MODEL_KINDS, kind)
}

/** One organisation's shared resources and models (user scope, "Shared with you"). */
export interface SharedOrgGroup {
  orgId: string
  orgName: string
  connections: AzureConnection[]
  /** In the registry's order (the store keeps it sorted). */
  models: AzureModelEntry[]
}

/**
 * The shared entries of user scope grouped by organisation, groups in name
 * order. Only organisations with models to show make a group: a shared
 * resource without models offers nothing to chat with.
 */
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

/** Where a count stands against its limit. */
export interface LimitUse {
  used: number
  limit: number
  /** At (or past) the limit: adding is refused. */
  reached: boolean
}

export interface LimitsView {
  connections: LimitUse
  models: LimitUse
  /** Show the "8 of 10 resources · 41 of 50 models" line: either count is at 80 % or more. */
  show: boolean
}

/** The owner's counts against the registry limits (10 resources, 50 models). */
export function limitsView(connections: number, models: number, limits: AzureRegistryLimits): LimitsView {
  const usage = (used: number, limit: number): LimitUse => ({used, limit, reached: limit > 0 && used >= limit})
  const near = (used: number, limit: number) => limit > 0 && used * 5 >= limit * 4
  return {
    connections: usage(connections, limits.connections),
    models: usage(models, limits.models),
    show: near(connections, limits.connections) || near(models, limits.models),
  }
}

/**
 * The model new chats start on, as AI Accounts marks it: the saved "Default
 * for new chats" when it names a listed entry, else the server's default.
 * Null when neither names one (a legacy saved id, say — the chat resolves
 * those through aliases).
 */
export function effectiveDefaultModelId(
  saved: string | null | undefined,
  serverDefault: string | null | undefined,
  models: readonly Pick<AzureModelEntry, 'modelId'>[],
): string | null {
  const listed = (id: string | null | undefined): id is string => !!id && models.some(m => m.modelId === id)
  if (listed(saved)) return saved
  return listed(serverDefault) ? serverDefault : null
}

// The namespace a deployment name is unique in on one resource: a chat and a
// Claude deployment share one (they are the same deployment on two routes),
// embeddings and agents have their own — the server's unique index. A kind
// this bundle does not know is its own, so it never reads as a duplicate.
function nameSpace(kind: Tolerant<AzureModelKind>): string {
  return kind === 'claude' ? 'chat' : kind
}

/**
 * The entry a new or edited model would duplicate (same resource, same name
 * space, same name compared case-insensitively), or undefined.
 */
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

/** Another model of the list with this display name (a warning only), or undefined. */
export function findDisplayNameTwin(
  models: readonly AzureModelEntry[],
  displayName: string,
  selfId?: string,
): AzureModelEntry | undefined {
  const name = displayName.trim().toLowerCase()
  if (!name) return undefined
  return models.find(m => m.id !== selfId && m.displayName.trim().toLowerCase() === name)
}

/** The discovered items not in the registry yet. */
export function unaddedItems(discovery: AzureDiscovery | null | undefined): AzureDiscoveryItem[] {
  if (!discovery) return []
  return [...discovery.deployments, ...discovery.agents].filter(item => !item.alreadyAdded)
}

/** How many discovered deployments and agents are not added yet. */
export function unaddedCounts(discovery: AzureDiscovery | null | undefined): {deployments: number; agents: number} {
  if (!discovery) return {deployments: 0, agents: 0}
  return {
    deployments: discovery.deployments.filter(d => !d.alreadyAdded).length,
    agents: discovery.agents.filter(a => !a.alreadyAdded).length,
  }
}

/** A stable key for a discovered item within its resource. */
export function discoveryItemKey(item: Pick<AzureDiscoveryItem, 'kind' | 'name'>): string {
  return `${item.kind === 'agent' ? 'agent' : 'deployment'}:${item.name.toLowerCase()}`
}

// ---- focus targets ---------------------------------------------------------
//
// The panel marks the elements it may move focus to with data-focus="<key>"
// and resolves a key after the render that follows an action.

export const focusKey = {
  /** A resource card's heading (tabIndex -1). */
  connection: (id: string) => `connection:${id}`,
  /** A resource card's "Add model" button. */
  addModel: (connectionId: string) => `add-model:${connectionId}`,
  /** A model row's name (tabIndex -1). */
  modelName: (id: string) => `model-name:${id}`,
  /** A model row's Edit button. */
  modelEdit: (id: string) => `model-edit:${id}`,
  /** The section's (or the empty state's) "Add resource" button. */
  addResource: 'add-resource',
  /** A resource's discovery Show/Hide toggle. */
  discoveryToggle: (connectionId: string) => `discovery-toggle:${connectionId}`,
  /** A discovered item's quick-add button (key: discoveryItemKey). */
  discoveryAdd: (connectionId: string, key: string) => `discovery-add:${connectionId}:${key}`,
}

/**
 * Where focus goes once a model row is removed: the next row's Edit, else
 * the previous row's, else the resource's "Add model". `rows` are the ids of
 * the resource's rows in display order, the removed one included.
 */
export function focusAfterModelRemoval(rows: readonly string[], removedId: string, connectionId: string): string {
  const at = rows.indexOf(removedId)
  const next = at >= 0 ? (rows[at + 1] ?? rows[at - 1]) : undefined
  return next ? focusKey.modelEdit(next) : focusKey.addModel(connectionId)
}

/**
 * Where focus goes once a resource is removed: the next card's heading, else
 * the previous card's, else "Add resource". `cards` are the resource ids in
 * display order, the removed one included.
 */
export function focusAfterConnectionRemoval(cards: readonly string[], removedId: string): string {
  const at = cards.indexOf(removedId)
  const next = at >= 0 ? (cards[at + 1] ?? cards[at - 1]) : undefined
  return next ? focusKey.connection(next) : focusKey.addResource
}
