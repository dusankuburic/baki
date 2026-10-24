import {z} from 'zod'
import {
  AUTH_TYPES,
  AZURE_AUTH_MODES,
  AZURE_MODEL_KINDS,
  AZURE_REGISTRY_STATES,
  AZURE_STATUSES,
  CREDENTIAL_FIELD_TYPES,
  CREDENTIAL_KINDS,
  PROVIDER_IDS,
  PROVIDER_MODES,
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
  type ProviderCatalog,
  type ProviderInfo,
  type ProviderSpec,
  type RuleStage,
  type Severity,
  type Tolerant,
  type UnknownEnumValue,
} from '@/types'
import {PURPOSE_PRESETS} from '@/lib/modelPurpose'
import type {AdminUser} from './admin'
import type {
  ChannelEvent,
  ChannelKind,
  GovernanceAlertSeverity,
  GovernanceAlertType,
  GovernanceNoticeScope,
} from './governance'
import type {LibraryFlow} from './library'

function nilAsEmpty<T extends z.ZodTypeAny>(item: T) {
  return z
    .array(item)
    .nullish()
    .transform(v => v ?? [])
}

function tolerantEnum<T extends string>(values: readonly T[]) {
  return z.string().transform((v): Tolerant<T> => (isKnown(values, v) ? v : (v as UnknownEnumValue)))
}

function nullAsAbsent<T extends z.ZodTypeAny>(item: T) {
  return item.nullish().transform(v => v ?? undefined)
}

export function buildSchemas() {
  const AuthUserSchema = z.object({
    id: z.string(),
    email: z.string(),
    role: z.enum(['admin', 'member', 'viewer', 'guest']),
    displayName: z.string().optional(),
    avatarUrl: z.string().optional(),
    createdAt: z.string().optional(),
    emailVerified: z.boolean().optional(),
    profile: z
      .object({
        bio: z.string().optional(),
        jobTitle: z.string().optional(),
        timezone: z.string().optional(),
      })
      .optional(),
  })

  const LoginResponseSchema = z.object({
    accessToken: z.string().min(1),
    refreshToken: z.string().min(1),
    expiresAt: z.string(),
    user: AuthUserSchema,
  })

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
      stage: tolerantEnum<RuleStage>(RULE_STAGES).optional(),
    })
    .passthrough()

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

  // A policy round-trips: the panel saves back what it listed and the server replaces the stored one wholesale, so
  // every level passes unknown fields through. The severities are tolerant: the server reads a gate it does not know
  // as report-only, an override it does not know as an error (analyzer.EvaluatePolicy).
  const PolicySeveritySchema = tolerantEnum<Severity>(['error', 'warning', 'info'])
  const PolicySchema = z
    .object({
      id: z.string(),
      orgId: z.string().optional(),
      name: z.string(),
      description: z.string().optional(),
      rules: nilAsEmpty(
        z
          .object({
            ruleId: z.string(),
            enabled: z.boolean(),
            severity: PolicySeveritySchema.optional(),
            maxCount: nullAsAbsent(z.number()),
          })
          .passthrough(),
      ),
      gateSeverity: PolicySeveritySchema.optional(),
      waivers: nullAsAbsent(
        z.array(
          z
            .object({findingKey: z.string(), reason: z.string().optional(), expiresAt: z.string().optional()})
            .passthrough(),
        ),
      ),
      maxErrors: nullAsAbsent(z.number()),
      maxWarnings: nullAsAbsent(z.number()),
      maxInfo: nullAsAbsent(z.number()),
      createdAt: z.string().optional(),
      updatedAt: z.string().optional(),
    })
    .passthrough()
  const PoliciesSchema = nilAsEmpty(PolicySchema)

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
      highlights: nilAsEmpty(HighlightSchema),
      flowId: z.string().optional(),
      flowName: z.string().optional(),
    })
    .passthrough()

  const SearchResultsSchema = z
    .object({
      query: z.object({text: z.string(), fuzzy: z.boolean(), maxResults: z.number()}).passthrough(),
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
      snapshots: nilAsEmpty(SnapshotInfoSchema),
    })
    .passthrough()

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

  const LibraryFlowsPageSchema = z
    .object({
      items: nilAsEmpty(LibraryFlowSchema),
      total: z.number(),
      offset: z.number(),
      limit: z.number(),
      hasMore: z.boolean().optional(),
      nextCursor: z.string().optional(),
      totalCapped: z.boolean().optional(),
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

  const LibraryFlowVersionsSchema = z.array(LibraryFlowVersionSchema)

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

  const OrgCICredentialSchema = z
    .object({
      id: z.string(),
      name: z.string(),
      secretHint: z.string().default(''),
      createdBy: z.string().default(''),
      createdByName: z.string().default(''),
      createdAt: z.string(),
      expiresAt: z.string().nullish(),
      lastUsedAt: z.string().nullish(),
    })
    .passthrough()

  const OrgCICredentialsSchema = nilAsEmpty(OrgCICredentialSchema)

  const CreatedOrgCICredentialSchema = OrgCICredentialSchema.extend({credential: z.string().min(1)}).passthrough()

  const channelKinds: ChannelKind[] = ['webhook', 'teams', 'teams-workflow', 'slack', 'jira', 'email']
  const channelEvents: ChannelEvent[] = ['drift', 'health_regression', 'new_rules']
  const OrgChannelSchema = z
    .object({
      id: z.string(),
      name: z.string().default(''),
      kind: tolerantEnum<ChannelKind>(channelKinds),
      url: z.string().default(''),
      enabled: z.boolean(),
      createdAt: z.string(),
      hasSecret: z.boolean().optional(),
      email: z.string().optional(),
      project: z.string().optional(),
      hasToken: z.boolean().optional(),
      recipients: z.array(z.string()).optional(),
      events: z.array(tolerantEnum<ChannelEvent>(channelEvents)).optional(),
      unreadable: z.boolean().optional(),
    })
    .passthrough()

  const OrgChannelsSchema = nilAsEmpty(OrgChannelSchema)

  const ChannelOptionsSchema = z
    .object({
      kinds: z.array(tolerantEnum<ChannelKind>(channelKinds)).default([]),
      events: z.array(tolerantEnum<ChannelEvent>(channelEvents)).default([]),
      defaultEvents: z.array(tolerantEnum<ChannelEvent>(channelEvents)).default([]),
      hostsRestricted: z.boolean().default(false),
      allowedHosts: z.array(z.string()).optional(),
    })
    .passthrough()

  const ModelPurposeSchema = tolerantEnum([...PURPOSE_PRESETS, 'custom'])
  const ModelDetailSchema = z
    .object({
      id: z.string(),
      displayName: z.string(),
      contextLimit: z.number(),
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

  const CredentialFieldSchema = z
    .object({
      name: z.string(),
      label: z.string(),
      type: tolerantEnum(CREDENTIAL_FIELD_TYPES),
      required: z.boolean(),
      placeholder: z.string().optional(),
      help: z.string().optional(),
      options: z.array(z.object({value: z.string(), label: z.string().optional()}).passthrough()).optional(),
    })
    .passthrough()

  const CredentialSchemaSchema = z
    .object({
      kind: tolerantEnum(CREDENTIAL_KINDS),
      fields: nilAsEmpty(CredentialFieldSchema),
      helpUrl: z.string().optional(),
    })
    .passthrough()

  const ProviderEndpointsSchema = z
    .object({
      save: z.string().optional(),
      test: z.string().optional(),
      start: z.string().optional(),
      poll: z.string().optional(),
      revoke: z.string().optional(),
      user: z.string().optional(),
    })
    .passthrough()
    .nullish()
    .transform(v => v ?? {})

  const providerDescription = {
    modes: nilAsEmpty(tolerantEnum(PROVIDER_MODES)),
    caps: nilAsEmpty(z.string()),
    credential: CredentialSchemaSchema,
    endpoints: ProviderEndpointsSchema,
    color: z.string().optional(),
    icon: z.string().optional(),
  }

  const ProviderInfoSchema = z
    .object({
      id: tolerantEnum(PROVIDER_IDS),
      name: z.string(),
      authType: tolerantEnum(AUTH_TYPES),
      contextLimit: z.number(),
      models: nilAsEmpty(ModelDetailSchema),
      defaultModel: z.string(),
      configured: z.boolean(),
      shared: z.boolean().optional(),
      suggestDiscovery: z.boolean().optional(),
      modes: providerDescription.modes.optional(),
      caps: providerDescription.caps.optional(),
      credential: providerDescription.credential.optional(),
      endpoints: providerDescription.endpoints.optional(),
      color: providerDescription.color,
      icon: providerDescription.icon,
    })
    .passthrough() as unknown as z.ZodType<ProviderInfo>

  const ProviderListSchema = nilAsEmpty(ProviderInfoSchema) as unknown as z.ZodType<ProviderInfo[]>

  const ProviderSpecSchema = z
    .object({
      id: z.string(),
      name: z.string(),
      authType: tolerantEnum(AUTH_TYPES),
      configured: z.boolean(),
      ...providerDescription,
    })
    .passthrough() as unknown as z.ZodType<ProviderSpec>

  const ProviderCatalogSchema = z
    .object({providers: nilAsEmpty(ProviderSpecSchema)})
    .passthrough() as unknown as z.ZodType<ProviderCatalog>

  const GitHubUserSchema = z.object({login: z.string(), name: z.string()}).passthrough()
  const GitHubUserOrNullSchema = z.union([GitHubUserSchema, z.null()])

  const DeviceAuthUserOrNullSchema = z.union([
    z
      .object({
        login: z.string().nullish(),
        name: z.string().nullish(),
        email: z.string().nullish(),
        id: z.union([z.string(), z.number()]).nullish(),
      })
      .passthrough(),
    z.null(),
  ])

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
      lastTest: AzureLastTestSchema.nullish(),
      updatedAt: z.string(),
      credentialUpdatedAt: z.string().optional(),
      sharedBy: z.string().optional(),
    })
    .passthrough()

  const AzureConnectionsListingSchema = z
    .object({
      connections: nilAsEmpty(AzureConnectionSchema),
      registryState: tolerantEnum(AZURE_REGISTRY_STATES),
      limits: z.object({connections: z.number(), models: z.number()}).passthrough(),
      knowledgeEmbeddingEnabled: z.boolean(),
      recoveredCopyPath: z.string().optional(),
    })
    .passthrough()

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

  const AzureModelsListingSchema = z
    .object({
      models: nilAsEmpty(AzureModelEntrySchema),
      serverDefault: z.string().optional(),
    })
    .passthrough() as unknown as z.ZodType<AzureModelsListing>

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

  const SuggestedPromptsSchema = z
    .array(z.string())
    .nullish()
    .transform(v => v ?? [])

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
      hasMore: z.boolean().optional(),
      nextCursor: z.string().optional(),
    })
    .passthrough() as unknown as z.ZodType<PagedResponse<AdminUser>>

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
      healthTrend: nilAsEmpty(z.object({date: z.string(), avgHealth: z.number(), flowCount: z.number()}).passthrough()),
      ruleFrequency: nilAsEmpty(z.object({rule: z.string(), count: z.number()}).passthrough()),
      activity: nilAsEmpty(z.object({action: z.string(), createdAt: z.string()}).passthrough()),
      complexity: nilAsEmpty(
        z
          .object({
            flowId: z.string(),
            flowName: z.string(),
            blockCount: z.number(),
            findingCount: z.number(),
            healthScore: z.number(),
          })
          .passthrough(),
      ),
      security: z
        .object({
          available: z.boolean(),
          failedLogins24h: z.number(),
          credentialFindings: z.number(),
          admin: z.boolean(),
        })
        .passthrough(),
      severityTrend: nilAsEmpty(
        z.object({date: z.string(), errors: z.number(), warnings: z.number(), info: z.number()}).passthrough(),
      ),
      confidenceDist: z
        .record(z.string(), z.number())
        .nullish()
        .transform(v => v ?? {}),
      healthBuckets: nilAsEmpty(
        z.object({label: z.string(), lo: z.number(), hi: z.number(), count: z.number()}).passthrough(),
      ),
      fixability: z
        .object({available: z.number(), total: z.number(), autoFixableRules: z.number(), totalRules: z.number()})
        .passthrough(),
      workflow: z.object({available: z.boolean()}).passthrough(),
    })
    .passthrough() as unknown as z.ZodType<DashboardHomeData>

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
    PolicySchema,
    PoliciesSchema,
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
    OrgCICredentialsSchema,
    CreatedOrgCICredentialSchema,
    OrgChannelsSchema,
    ChannelOptionsSchema,
    ProviderListSchema,
    ProviderCatalogSchema,
    ProviderInfoSchema,
    GitHubUserOrNullSchema,
    DeviceAuthUserOrNullSchema,
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
