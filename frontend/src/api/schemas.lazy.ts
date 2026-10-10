import {z} from 'zod'
import {
  AUTH_TYPES,
  AZURE_AUTH_MODES,
  AZURE_MODEL_KINDS,
  AZURE_REGISTRY_STATES,
  AZURE_STATUSES,
  PROVIDER_IDS,
  RULE_STAGES,
  isKnown,
  type AnalysisReport,
  type AppSettings,
  type AzureConnection,
  type AzureModelKind,
  type AzureModelsListing,
  type DashboardAnalyticsData,
  type DashboardHomeData,
  type FlowDocument,
  type PagedResponse,
  type ProviderInfo,
  type RuleStage,
  type Tolerant,
  type UnknownEnumValue,
} from '@/types'
import {PURPOSE_PRESETS} from '@/lib/modelPurpose'
import type {AdminUser} from './admin'
import type {GovernanceAlertSeverity, GovernanceAlertType, GovernanceNoticeScope} from './governance'
import type {LibraryFlow} from './library'

// A Go server serialises a nil slice (or map) as null — an empty list on the
// wire is `null`, not `[]` (providers.ts's list() helper guards the same case
// at call sites). Accept null/undefined and read it as empty so a
// legitimately-empty list validates instead of throwing ResponseValidationError.
function nilAsEmpty<T extends z.ZodTypeAny>(item: T) {
  return z.array(item).nullish().transform(v => v ?? [])
}

// A wire enum the backend may grow (types/tolerant.ts): a known value keeps
// its literal type, and any other string parses as itself, typed
// UnknownEnumValue, instead of failing the whole response. z.enum stays only
// where an unknown value must not be guessed at: the roles, which gate the UI,
// and Finding.severity, which the backend closes itself and the client ranks.
function tolerantEnum<T extends string>(values: readonly T[]) {
  return z.string().transform((v): Tolerant<T> => (isKnown(values, v) ? v : (v as UnknownEnumValue)))
}

// This module — and therefore zod — is ONLY reachable through the dynamic
// import('./schemas.lazy') in ./schemas.ts. Keeping zod's `z` binding inside
// one chunk lets rollup's deep tree-shaking prune unused members (passing `z`
// across a chunk boundary, or as a function parameter, ships the entire
// library ~4× larger).

export function buildSchemas() {
  // ── Auth ─────────────────────────────────────────────────────────────────
  const AuthUserSchema = z.object({
    id: z.string(),
    email: z.string(),
    // Closed: a role this bundle does not know is refused, never guessed
    // into a privilege; a new role ships with the bundle that knows it.
    role: z.enum(['admin', 'member', 'viewer', 'guest']),
    displayName: z.string().optional(),
    avatarUrl: z.string().optional(),
    createdAt: z.string().optional(),
  })

  const LoginResponseSchema = z.object({
    accessToken: z.string().min(1),
    refreshToken: z.string().min(1),
    expiresAt: z.string(),
    user: AuthUserSchema,
  })

  // Register is NOT a login: unless the account was created already verified
  // (the bootstrap admin), the server sends a verification link and returns
  // no tokens. Validating it against LoginResponseSchema made every ordinary
  // sign-up fail client-side after the account had been created.
  const RegisterResponseSchema = z.object({
    status: z.string().optional(),
    user: AuthUserSchema,
    verificationRequired: z.boolean(),
    accessToken: z.string().min(1).optional(),
    refreshToken: z.string().min(1).optional(),
    expiresAt: z.string().optional(),
  })

  const RefreshResponseSchema = z.object({
    accessToken: z.string().min(1),
    expiresAt: z.string(),
    refreshToken: z.string().optional(),
  })

  // ── Analysis ──────────────────────────────────────────────────────────────
  // AnalysisReport is the core product output; an empty/missing findings array
  // would silently render a "clean" flow when the backend actually errored.
  //
  // Both schemas use .passthrough() so backend fields NOT explicitly listed
  // here (autoFix, fingerprint, confidence, autoFixHint, metadata on Finding;
  // metrics, groups, ruleProfiles on AnalysisReport) survive validation.
  // Without passthrough, zod's default .strip() would silently delete them and
  // break the Apply-fix button, health-score badge, per-rule timing,
  // confidence badges, and triage/suppression keying.

  // Envelope check for the document a `flow:loaded` SSE event triggered a
  // re-fetch of (useAppEvents). The event itself carries only a lightweight
  // summary; the fetched document is validated here before it enters the
  // editor store. Same philosophy as above: validate the top-level shape so
  // a malformed payload (HTML error page, truncated JSON, a different
  // event's data) is rejected before it can hijack the editor, while
  // passthrough keeps the deep block tree untouched.
  const FlowDocumentEnvelopeSchema = z
    .object({
      id: z.string().min(1),
      name: z.string(),
      subflows: z.array(z.object({id: z.string(), name: z.string()}).passthrough()).min(1),
    })
    .passthrough()

  const FindingSchema = z
    .object({
      id: z.string(),
      ruleId: z.string(),
      severity: z.enum(['error', 'warning', 'info']),
      title: z.string(),
      description: z.string(),
      blockId: z.string().optional(),
      subflowId: z.string().optional(),
      suggestion: z.string().optional(),
      category: z.string().optional(),
      // 'preview' from a preview rule; a stage a newer server adds parses as itself.
      stage: tolerantEnum<RuleStage>(RULE_STAGES).optional(),
    })
    .passthrough()

  // The loads (load-path / load-folder / library content) answer a full
  // parsed document, so unlike the lightweight flow:loaded envelope check
  // above, require the parser-emitted filePath + metadata too (the Go struct
  // marshals both unconditionally). Everything deeper passes through — the
  // block tree is the editor's domain, not the boundary's.
  const LoadedFlowDocumentSchema = z
    .object({
      id: z.string().min(1),
      name: z.string(),
      filePath: z.string(),
      metadata: z.object({}).passthrough(),
      subflows: z.array(z.object({id: z.string(), name: z.string()}).passthrough()).min(1),
    })
    .passthrough() as unknown as z.ZodType<FlowDocument>

  const AnalysisReportSchema = z
    .object({
      flowId: z.string(),
      generatedAt: z.string(),
      durationMs: z.number(),
      findings: z.array(FindingSchema),
      // stats REQUIRED (F1.9): the analyzer unconditionally stamps it
      // (core/analyzer/engine.go computeStats), and the TS type requires it —
      // optionality here only hid the drift behind the cast below.
      stats: z
        .object({
          errors: z.number(),
          warnings: z.number(),
          info: z.number(),
          blocksAnalyzed: z.number(),
          rulesRun: z.number(),
        })
        .passthrough(),
    })
    .passthrough() as unknown as z.ZodType<AnalysisReport>

  // ── Settings ──────────────────────────────────────────────────────────────
  // AppSettings is deeply nested; rather than duplicate the entire type tree
  // in zod, validate the top-level shape (version + the known section keys)
  // and let zod passthrough the rest. This still catches the real failure
  // modes: an HTML proxy body, null, or a fundamentally wrong shape.
  // Annotated as ZodType<AppSettings> so requestValidated<AppSettings>
  // accepts it.
  const AppSettingsSchema = z
    .object({
      version: z.number(),
      general: z.object({}).passthrough(),
      appearance: z.object({}).passthrough(),
      layout: z.object({}).passthrough(),
      ai: z.object({}).passthrough(),
      parser: z.object({}).passthrough(),
      analysis: z.object({}).passthrough(),
    })
    .passthrough() as unknown as z.ZodType<AppSettings>

  // ── Flow reads (recent files, search, source files/meta, snapshots) ──────
  // The desktop list endpoints answer `null` for an empty set (Go nil slice,
  // e.g. RecentFiles with no history), so the top-level arrays run through
  // nilAsEmpty. Per-item fields mirror the Go structs in
  // internal/storage/interfaces / core/models.
  const RecentFileSchema = z
    .object({
      path: z.string(),
      name: z.string(),
      size: z.number(),
      lastOpen: z.string(),
      isFolder: z.boolean().optional(),
    })
    .passthrough()

  const RecentFilesSchema = nilAsEmpty(RecentFileSchema)

  const HighlightSchema = z.object({start: z.number(), end: z.number()}).passthrough()

  const SearchResultSchema = z
    .object({
      blockId: z.string(),
      subflowId: z.string(),
      matchedField: z.string(),
      matchedText: z.string(),
      score: z.number(),
      // computeHighlights returns nil when a match has no highlights → null.
      highlights: nilAsEmpty(HighlightSchema),
      // Stamped only by cross-library search (search-library).
      flowId: z.string().optional(),
      flowName: z.string().optional(),
    })
    .passthrough()

  const SearchResultsSchema = z
    .object({
      query: z.object({text: z.string(), fuzzy: z.boolean(), maxResults: z.number()}).passthrough(),
      // A search with no matches marshals the nil slice as null.
      results: nilAsEmpty(SearchResultSchema),
      totalCount: z.number(),
      durationMs: z.number(),
    })
    .passthrough()

  const SourceFileInfoSchema = z
    .object({
      filename: z.string(),
      subflowId: z.string(),
      subflowName: z.string(),
      blockCount: z.number(),
      lineCount: z.number(),
    })
    .passthrough()

  // GetSourceFiles answers nil (→ null) when no document is loaded.
  const SourceFilesSchema = nilAsEmpty(SourceFileInfoSchema)

  const SourceMetaSchema = z
    .object({
      size: z.number(),
      modTime: z.string(),
      files: z.number(),
    })
    .passthrough()

  const SnapshotInfoSchema = z
    .object({
      id: z.string(),
      label: z.string(),
      createdAt: z.string(),
      bytes: z.number(),
    })
    .passthrough()

  const SnapshotsListSchema = z
    .object({
      // ListSourceSnapshots answers nil before the first snapshot → null.
      snapshots: nilAsEmpty(SnapshotInfoSchema),
    })
    .passthrough()

  // ── Library ───────────────────────────────────────────────────────────────
  // Mirrors api.libraryFlow (internal/api/library.go): the permission flags
  // and counts are always marshalled, the description/owner-name/tags and the
  // health trio are omitempty (health only on the single-flow GET).
  const LibraryFlowSchema = z
    .object({
      id: z.string(),
      name: z.string(),
      description: z.string().optional(),
      ownerId: z.string(),
      ownerDisplayName: z.string().optional(),
      organizationId: z.string().optional(),
      isSharedWithMe: z.boolean(),
      canEdit: z.boolean(),
      canDelete: z.boolean(),
      canShare: z.boolean(),
      blockCount: z.number(),
      subflowCount: z.number(),
      updatedAt: z.string(),
      version: z.number(),
      tags: z.array(z.string()).optional(),
      healthScore: z.number().optional(),
      errorCount: z.number().optional(),
      warningCount: z.number().optional(),
    })
    .passthrough()

  // render.PagedResponse[libraryFlow]: {items, total, offset, limit}.
  const LibraryFlowsPageSchema = z
    .object({
      items: nilAsEmpty(LibraryFlowSchema),
      total: z.number(),
      offset: z.number(),
      limit: z.number(),
    })
    .passthrough() as unknown as z.ZodType<PagedResponse<LibraryFlow>>

  const LibraryFlowVersionSchema = z
    .object({
      id: z.string(),
      flowId: z.string(),
      version: z.number(),
      comment: z.string(),
      createdBy: z.string(),
      createdAt: z.string(),
    })
    .passthrough()

  // toFlowVersionViews builds a non-nil slice; metadata (block/subflow counts)
  // rides along via passthrough.
  const LibraryFlowVersionsSchema = z.array(LibraryFlowVersionSchema)

  // ── Governance alerts ─────────────────────────────────────────────────────
  // Mirrors storageif.GovernanceAlert. `type`/`severity` are tolerant: the
  // server writes the title and message, so an alert of a newer type still
  // reads, and one such row must not empty the whole inbox.
  const GovernanceAlertSchema = z
    .object({
      id: z.string(),
      flowId: z.string(),
      flowName: z.string().optional(),
      orgId: z.string().optional(),
      type: tolerantEnum<GovernanceAlertType>([
        'drift',
        'health_regression',
        'finding_assigned',
        'comment_added',
        'new_rules',
      ]),
      title: z.string(),
      message: z.string().optional(),
      severity: tolerantEnum<GovernanceAlertSeverity>(['error', 'warning']),
      newErrors: z.number().optional(),
      newWarnings: z.number().optional(),
      healthScore: z.number().optional(),
      prevHealth: z.number().optional(),
      createdAt: z.string(),
      // *time.Time pointers, omitempty on the backend; the hand-written type
      // keeps `| null` for older payloads that marshalled them as null.
      readAt: z.string().nullish(),
      dismissedAt: z.string().nullish(),
      targetUser: z.string().optional(),
      ruleIds: z.array(z.string()).optional(),
      scope: tolerantEnum<GovernanceNoticeScope>(['org', 'user', 'app']).optional(),
      flowCount: z.number().optional(),
    })
    .passthrough()

  const GovernanceAlertsSchema = nilAsEmpty(GovernanceAlertSchema)

  const UnreadCountSchema = z.object({count: z.number()}).passthrough()

  // ── Providers ─────────────────────────────────────────────────────────────
  // Mirrors models.ProviderInfo/ModelDetail (core/models/chat.go). The ids,
  // kinds and statuses are tolerant: the backend may add a provider (or a
  // model kind) before the client's PROVIDERS catalogue learns it, and that
  // must degrade to one entry the chat leaves out — a strict enum would fail
  // the whole list and blank the chat picker.
  const ModelPurposeSchema = tolerantEnum([...PURPOSE_PRESETS, 'custom'])
  const ModelDetailSchema = z
    .object({
      id: z.string(),
      displayName: z.string(),
      contextLimit: z.number(),
      inputCostPerM: z.number(),
      outputCostPerM: z.number(),
      supportsTools: z.boolean(),
      kind: tolerantEnum(AZURE_MODEL_KINDS).optional(),
      purpose: ModelPurposeSchema.optional(),
      purposeCustom: z.string().optional(),
      description: z.string().optional(),
      shared: z.boolean().optional(),
      orgId: z.string().optional(),
      orgName: z.string().optional(),
      connectionId: z.string().optional(),
      connectionName: z.string().optional(),
      status: tolerantEnum(AZURE_STATUSES).optional(),
      statusReason: z.string().optional(),
      aliases: z.array(z.string()).optional(),
      isDefault: z.boolean().optional(),
      isOrgDefault: z.boolean().optional(),
    })
    .passthrough()

  const ProviderInfoSchema = z
    .object({
      id: tolerantEnum(PROVIDER_IDS),
      name: z.string(),
      authType: tolerantEnum(AUTH_TYPES),
      contextLimit: z.number(),
      // Catalogue/live/registry listings all send a non-nil models slice, but
      // the registry path assembles it lazily — tolerate null.
      models: nilAsEmpty(ModelDetailSchema),
      defaultModel: z.string(),
      configured: z.boolean(),
      shared: z.boolean().optional(),
      suggestDiscovery: z.boolean().optional(),
    })
    .passthrough() as unknown as z.ZodType<ProviderInfo>

  const ProviderListSchema = nilAsEmpty(ProviderInfoSchema) as unknown as z.ZodType<ProviderInfo[]>

  // GET /api/providers/github/user (and the Copilot twin) answers null when
  // not connected — "no stored token" is not an error.
  const GitHubUserSchema = z.object({login: z.string(), name: z.string()}).passthrough()
  const GitHubUserOrNullSchema = z.union([GitHubUserSchema, z.null()])

  const AzureStatusSchema = tolerantEnum(AZURE_STATUSES)

  const AzureLastTestSchema = z
    .object({
      ok: z.boolean(),
      latencyMs: z.number(),
      error: z.string().optional(),
      at: z.string(),
    })
    .passthrough()

  const AzureConnectionSchema = z
    .object({
      id: z.string(),
      name: z.string(),
      shared: z.boolean(),
      orgId: z.string().optional(),
      orgName: z.string().optional(),
      legacy: z.boolean(),
      config: z
        .object({
          endpoint: z.string(),
          // '' when the stored credential document is missing/unreadable.
          authMode: tolerantEnum([...AZURE_AUTH_MODES, '']),
          hasSecret: z.boolean(),
          tenantId: z.string().optional(),
          clientId: z.string().optional(),
          projectEndpoint: z.string().optional(),
        })
        .passthrough(),
      status: AzureStatusSchema,
      statusReason: z.string().optional(),
      canDiscover: z.boolean(),
      supportsAgents: z.boolean(),
      suggestDiscovery: z.boolean(),
      modelCount: z.number(),
      // *AzureLastTestView (no omitempty): nil marshals as null.
      lastTest: AzureLastTestSchema.nullish(),
      updatedAt: z.string(),
      credentialUpdatedAt: z.string().optional(),
      sharedBy: z.string().optional(),
    })
    .passthrough()

  // GET /connections (internal/service/azure_models.go AzureConnectionsListing).
  const AzureConnectionsListingSchema = z
    .object({
      connections: nilAsEmpty(AzureConnectionSchema),
      registryState: tolerantEnum(AZURE_REGISTRY_STATES),
      limits: z.object({connections: z.number(), models: z.number()}).passthrough(),
      knowledgeEmbeddingEnabled: z.boolean(),
      recoveredCopyPath: z.string().optional(),
    })
    .passthrough()

  // GET /connections/{id}: the backend always wraps in {connection}, but the
  // client has always accepted the bare resource too — keep both, lift the
  // envelope. The transform output is the resource; the cast names it.
  const AzureConnectionResponseSchema = z
    .union([z.object({connection: AzureConnectionSchema}).passthrough(), AzureConnectionSchema])
    .transform(v => ('connection' in v && v.connection ? v.connection : v)) as unknown as z.ZodType<AzureConnection>

  const AzureModelEntrySchema = z
    .object({
      id: z.string(),
      modelId: z.string(),
      connectionId: z.string(),
      connectionName: z.string(),
      kind: tolerantEnum(AZURE_MODEL_KINDS),
      deployment: z.string(),
      target: z.string(),
      displayName: z.string(),
      // Purpose is a preset key or 'custom'; the server may learn a new preset
      // first — a strict enum would fail the whole listing.
      purpose: ModelPurposeSchema,
      purposeCustom: z.string().optional(),
      description: z.string().optional(),
      contextWindow: z.number().optional(),
      reportedContextWindow: z.number().optional(),
      maxOutputTokens: z.number().optional(),
      isDefault: z.boolean(),
      isKnowledgeEmbedding: z.boolean(),
      shared: z.boolean(),
      orgId: z.string().optional(),
      orgName: z.string().optional(),
      status: AzureStatusSchema,
      statusReason: z.string().optional(),
      lastTest: AzureLastTestSchema.nullish(),
      updatedAt: z.string(),
    })
    .passthrough()

  // GET /models (AzureModelsListing).
  const AzureModelsListingSchema = z
    .object({
      models: nilAsEmpty(AzureModelEntrySchema),
      serverDefault: z.string().optional(),
    })
    .passthrough() as unknown as z.ZodType<AzureModelsListing>

  // POST /connections/{id}/discover (AzureDiscoveryView). Unlike the frontend
  // types the backend always marshals kind/model/alreadyAdded on each item, so
  // the client-side fill-in (discoveryItems) can go once this validates.
  const AzureDiscoveryItemSchema = z
    .object({
      name: z.string(),
      kind: tolerantEnum(AZURE_MODEL_KINDS),
      model: z.string(),
      contextWindow: z.number().optional(),
      alreadyAdded: z.boolean(),
      modelId: z.string().optional(),
      deployment: z.string().optional(),
      workflow: z.boolean().optional(),
    })
    .passthrough()

  // Suggestions to add: one of a kind this bundle does not know could not be
  // added from here (the create request takes known kinds only), so it is
  // left out rather than offered.
  const knownKind = <I extends {kind: Tolerant<AzureModelKind>}>(item: I): item is I & {kind: AzureModelKind} =>
    isKnown(AZURE_MODEL_KINDS, item.kind)
  const AzureDiscoveryItemsSchema = nilAsEmpty(AzureDiscoveryItemSchema).transform(items => items.filter(knownKind))

  const AzureDiscoverySchema = z
    .object({
      deployments: AzureDiscoveryItemsSchema,
      agents: AzureDiscoveryItemsSchema,
      warnings: nilAsEmpty(z.string()),
      error: z.string().optional(),
      code: z.string().optional(),
    })
    .passthrough()

  // ── Chat ──────────────────────────────────────────────────────────────────
  // /api/chat/suggested-prompts: the prompts come from settings lists that are
  // empty on a fresh install → nil slice → null. getConversation keeps its own
  // validator (parseConversationLoad): the payload is a bare array OR an
  // {messages, activeStreamId} object and per-message coercion lives in
  // lib/chatMessage — a zod duplicate would only drift from it.
  const SuggestedPromptsSchema = z.array(z.string()).nullish().transform(v => v ?? [])

  // ── Admin ─────────────────────────────────────────────────────────────────
  // Mirrors storageif.User as the admin list marshals it: every field is
  // always present (no omitempty); `role` is '' for accounts written before
  // the role check existed.
  const AdminUserSchema = z
    .object({
      id: z.string(),
      email: z.string(),
      role: z.union([z.enum(['admin', 'member', 'viewer', 'guest']), z.literal('')]),
      emailVerified: z.boolean(),
      displayName: z.string(),
      avatarUrl: z.string(),
      createdAt: z.string(),
      updatedAt: z.string(),
    })
    .passthrough()

  const AdminUsersPageSchema = z
    .object({
      items: nilAsEmpty(AdminUserSchema),
      total: z.number(),
      offset: z.number(),
      limit: z.number(),
    })
    .passthrough() as unknown as z.ZodType<PagedResponse<AdminUser>>

  // userStatsView: the five role buckets (incl. 'none') are always marshalled.
  const UserStatsSchema = z
    .object({
      total: z.number(),
      byRole: z
        .object({admin: z.number(), member: z.number(), viewer: z.number(), guest: z.number(), none: z.number()})
        .passthrough(),
      unverified: z.number(),
      locked: z.number(),
    })
    .passthrough()

  // auditEventView: the meta map is nil → null when the event carried none.
  const AuditEventSchema = z
    .object({
      id: z.string(),
      userId: z.string(),
      email: z.string(),
      action: z.string(),
      resourceType: z.string(),
      resourceId: z.string(),
      ip: z.string(),
      meta: z.record(z.string(), z.string()).nullable(),
      createdAt: z.string(),
    })
    .passthrough()

  const AuditEventsSchema = z.array(AuditEventSchema)
  const AuditActionsSchema = z.array(z.string())

  // ── Dashboard ─────────────────────────────────────────────────────────────
  // models.DashboardHomeData marshals every section key (no omitempty), but a
  // section with no data carries null slices/maps (Go nil). Same philosophy as
  // AppSettingsSchema: validate the load-bearing scalars the UI reads first,
  // passthrough the deep aggregations, read nil collections as empty.
  const DashboardHomeSchema = z
    .object({
      greeting: z.object({userDisplayName: z.string()}).passthrough(),
      overview: z
        .object({
          avgHealthScore: z.number(),
          healthAvailable: z.boolean(),
          totalFlows: z.number(),
          totalSubflows: z.number(),
        })
        .passthrough(),
      tokenUsage: nilAsEmpty(z.object({date: z.string(), tokensIn: z.number(), tokensOut: z.number()}).passthrough()),
      recentFlows: nilAsEmpty(
        z
          .object({id: z.string(), name: z.string(), healthScore: z.number().nullable(), updatedAt: z.string()})
          .passthrough(),
      ),
      findings: z.object({available: z.boolean()}).passthrough(),
      isCloud: z.boolean(),
      // Advanced sections (v2).
      healthTrend: nilAsEmpty(z.object({date: z.string(), avgHealth: z.number(), flowCount: z.number()}).passthrough()),
      costByProvider: nilAsEmpty(
        z.object({provider: z.string(), cost: z.number(), tokensIn: z.number(), tokensOut: z.number()}).passthrough(),
      ),
      ruleFrequency: nilAsEmpty(z.object({rule: z.string(), count: z.number()}).passthrough()),
      activity: nilAsEmpty(z.object({action: z.string(), createdAt: z.string()}).passthrough()),
      complexity: nilAsEmpty(
        z
          .object({flowId: z.string(), flowName: z.string(), blockCount: z.number(), findingCount: z.number(), healthScore: z.number()})
          .passthrough(),
      ),
      security: z
        .object({available: z.boolean(), failedLogins24h: z.number(), credentialFindings: z.number(), admin: z.boolean()})
        .passthrough(),
      // Analytics sections (v3).
      severityTrend: nilAsEmpty(
        z.object({date: z.string(), errors: z.number(), warnings: z.number(), info: z.number()}).passthrough(),
      ),
      confidenceDist: z.record(z.string(), z.number()).nullish().transform(v => v ?? {}),
      healthBuckets: nilAsEmpty(z.object({label: z.string(), lo: z.number(), hi: z.number(), count: z.number()}).passthrough()),
      fixability: z
        .object({available: z.number(), total: z.number(), autoFixableRules: z.number(), totalRules: z.number()})
        .passthrough(),
      workflow: z.object({available: z.boolean()}).passthrough(),
    })
    .passthrough() as unknown as z.ZodType<DashboardHomeData>

  // models.DashboardAnalyticsData: every section key is always marshalled and
  // the trend/partial slices are built non-nil (dashboard_analytics.go).
  // Sections are passthrough — their deep aggregations belong to the backend.
  const DashboardAnalyticsSchema = z
    .object({
      applied: z.object({scope: z.string(), compare: z.boolean(), derived: z.boolean()}).passthrough(),
      range: z.object({preset: z.string(), from: z.string(), to: z.string()}).passthrough(),
      summary: z.object({asOf: z.string(), flowsInScope: z.number(), unanalyzed: z.number()}).passthrough(),
      trend: z
        .object({
          points: z.array(z.object({date: z.string()}).passthrough()),
          prevPoints: z.array(z.object({date: z.string()}).passthrough()),
        })
        .passthrough(),
      findings: z.object({}).passthrough(),
      flows: z.object({}).passthrough(),
      triage: z.object({}).passthrough(),
      ai: z.object({}).passthrough(),
      activity: z.object({}).passthrough(),
      security: z.object({available: z.boolean()}).passthrough(),
      facets: z.object({flowsTruncated: z.boolean()}).passthrough(),
      partial: nilAsEmpty(z.string()),
      generatedAt: z.string(),
    })
    .passthrough() as unknown as z.ZodType<DashboardAnalyticsData>

  // ── System ────────────────────────────────────────────────────────────────
  // models.AppInfo: all fields always marshalled.
  const AppInfoSchema = z
    .object({
      version: z.string(),
      platform: z.string(),
      arch: z.string(),
      buildDate: z.string(),
      gitCommit: z.string(),
      capabilities: z.object({sessionAnalytics: z.boolean()}).passthrough(),
    })
    .passthrough()

  return {
    AuthUserSchema,
    LoginResponseSchema,
    RegisterResponseSchema,
    RefreshResponseSchema,
    FlowDocumentEnvelopeSchema,
    LoadedFlowDocumentSchema,
    FindingSchema,
    AnalysisReportSchema,
    AppSettingsSchema,
    RecentFilesSchema,
    SearchResultsSchema,
    SourceFilesSchema,
    SourceMetaSchema,
    SnapshotsListSchema,
    LibraryFlowSchema,
    LibraryFlowsPageSchema,
    LibraryFlowVersionsSchema,
    GovernanceAlertsSchema,
    UnreadCountSchema,
    ProviderListSchema,
    ProviderInfoSchema,
    GitHubUserOrNullSchema,
    AzureConnectionsListingSchema,
    AzureConnectionResponseSchema,
    AzureModelsListingSchema,
    AzureDiscoverySchema,
    SuggestedPromptsSchema,
    AdminUsersPageSchema,
    UserStatsSchema,
    AuditEventsSchema,
    AuditActionsSchema,
    DashboardHomeSchema,
    DashboardAnalyticsSchema,
    AppInfoSchema,
  }
}
