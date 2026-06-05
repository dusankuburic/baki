export type BlockType =
  | 'ACTION' | 'LOOP' | 'CONDITION' | 'SUBFLOW'
  | 'ERROR_HANDLER' | 'COMMENT' | 'VARIABLE' | 'WAIT' | 'BLOCK' | 'SWITCH' | 'ELSE' | 'CASE' | 'DEFAULT' | 'END' | 'UNKNOWN';

export type ProviderID = 'claude' | 'openai' | 'gemini' | 'xai' | 'glm' | 'github-models' | 'copilot' | 'demo';

export type AuthType = 'api_key' | 'oauth';

export type ThemeMode = 'dark' | 'light' | 'system' | 'midnight' | 'warm' | 'tokyo-night' | 'one-dark' | 'dracula' | 'nord';

export interface BlockToken {
  type: 'text' | 'variable' | 'subflow' | 'label' | 'string';
  value: string;
  target?: string;
}

export type ChangeType = 'none' | 'added' | 'removed' | 'modified';

export interface FlowDiff {
  oldId: string;
  newId: string;
  subflows: SubflowDiff[];
}

export interface SubflowDiff {
  name: string;
  change: ChangeType;
  blocks: BlockDiff[];
}

export interface BlockDiff {
  change: ChangeType;
  old?: Block;
  new?: Block;
}

export interface Block {
  id: string;
  name: string;
  type: BlockType;
  rawType: string;
  indent: number;
  lineNumber: number;
  children: Block[];
  properties: Record<string, string>;
  variables: string[];
  tokens?: BlockToken[];
  parentId?: string;
  subflowId: string;
}

export interface Subflow {
  id: string;
  name: string;
  sourceFile?: string; // file name (with .txt) the subflow was parsed from, only set for folder loads
  blocks: Block[];
  variables: VariableDecl[];
}

export interface VariableHistory {
  name: string;
  events: VariableEvent[];
}

export interface VariableEvent {
  type: 'init' | 'mutate' | 'read';
  blockId: string;
  line: number;
  subflowId: string;
}

export interface GraphNode {
  id: string;
  label: string;
  type: string;
  blockCount: number;
  errorCount: number;
  warnCount: number;
}

export interface GraphEdge {
  source: string;
  target: string;
}

export interface GraphData {
  nodes: GraphNode[];
  edges: GraphEdge[];
}

export interface VariableDecl {
  name: string;
  type: string;
  initialValue?: string;
  scope: 'flow' | 'subflow';
}

export interface FlowDocument {
  id: string;
  name: string;
  filePath: string;
  subflows: Subflow[];
  metadata: FlowMetadata;
  files?: FlowFileInfo[];
  isFolder?: boolean;
}

export interface FlowMetadata {
  blockCount: number;
  subflowCount: number;
  maxDepth: number;
  parsedAt: string;
  fileSize: number;
  rawLineCount: number;
}

export interface ParseError {
  line: number;
  column?: number;
  message: string;
  severity: 'warning' | 'error';
  snippet: string;
}

export interface RecentFile {
  path: string;
  name: string;
  size: number;
  lastOpen: string;
  isFolder?: boolean;
}

export interface FlowFileInfo {
  path: string;
  name: string;
  size: number;
}

export type Severity = 'error' | 'warning' | 'info';

export interface Finding {
  id: string;
  ruleId: string;
  severity: Severity;
  title: string;
  description: string;
  blockId: string;
  subflowId: string;
  suggestion?: string;
  autoFixHint?: string;
  category?: string;
  metadata?: Record<string, unknown>;
}

export interface RuleProfile {
  ruleId: string;
  ruleName: string;
  durationMs: number;
  findingCount: number;
  blocksChecked: number;
}

export interface AnalysisSnapshot {
  timestamp: string;
  flowId: string;
  hash: string;
  errors: number;
  warnings: number;
  info: number;
  healthScore: number;
  durationMs: number;
}

export interface AnalysisReport {
  flowId: string;
  generatedAt: string;
  findings: Finding[];
  stats: AnalysisStats;
  durationMs: number;
  metrics?: FlowMetrics;
  ruleProfiles?: RuleProfile[];
}

export interface AnalysisStats {
  errors: number;
  warnings: number;
  info: number;
  blocksAnalyzed: number;
  rulesRun: number;
}

export interface SubflowMetrics {
  subflowId: string;
  subflowName: string;
  blockCount: number;
  cyclomaticComplexity: number;
  cognitiveComplexity: number;
  maxNestingDepth: number;
  variableCount: number;
  fanIn: number;
  fanOut: number;
}

export interface FlowMetrics {
  subflows: SubflowMetrics[];
  totalBlocks: number;
  totalVariables: number;
  maxCyclomatic: number;
  avgCyclomatic: number;
  maxCognitive: number;
  avgCognitive: number;
  healthScore: number;
  variableDensity: number;
  subflowCount: number;
  circularDependencies?: string[];
}

export interface BlockDataFlow {
  blockId: string;
  subflowId: string;
  reads: string[];
  writes: string[];
  upstreamBlocks: string[];
  downstreamBlocks: string[];
}

export interface TaintPath {
  sourceVar: string;
  sourceBlock: string;
  sinkBlock: string;
  sinkType: string;
  path: string[];
}

export interface DeadDataPath {
  variable: string;
  setBlock: string;
  readBlock: string;
  reason: string;
}

export interface DataFlowAnalysis {
  blocks: Record<string, BlockDataFlow>;
  taintPaths: TaintPath[];
  deadData: DeadDataPath[];
}

export interface BatchResult {
  flowId: string;
  flowName: string;
  report: AnalysisReport;
  error?: string;
}

export interface BatchAnalysis {
  results: BatchResult[];
  totalFlows: number;
  totalFindings: number;
  totalErrors: number;
  totalWarnings: number;
  totalInfo: number;
  avgHealthScore: number;
  durationMs: number;
}

export interface AnalysisDiff {
  flowId: string;
  added: Finding[];
  removed: Finding[];
  persisted: Finding[];
  addedCount: number;
  removedCount: number;
  persistedCount: number;
}

export interface RuleDependency {
  fromRuleId: string;
  toRuleId: string;
  reason: string;
}

export interface DependencyAnalysis {
  dependencies: RuleDependency[];
  cycles: string[][];
  topoOrder: string[];
}

export interface SubflowHash {
  subflowId: string;
  hash: string;
}

export interface DashboardStats {
  totalFlowsAnalyzed: number;
  totalFindings: number;
  findingsBySeverity: Record<string, number>;
  findingsByCategory: Record<string, number>;
  findingsByRule: Record<string, number>;
  avgHealthScore: number;
  topProblemFlows: ProblemFlow[];
}

export interface ProblemFlow {
  flowId: string;
  flowName: string;
  findingCount: number;
  healthScore: number;
}

export interface FindingGroup {
  blockId: string;
  findings: Finding[];
  primary: Finding;
  duplicateCount: number;
}

export interface DeduplicateResult {
  deduplicated: Finding[];
  groups: FindingGroup[];
  originalCount: number;
  dedupedCount: number;
}

export interface FlowComparison {
  flowAId: string;
  flowBId: string;
  subflowDiff: SubflowComparison[];
  sharedBlocks: number;
  addedBlocks: number;
  removedBlocks: number;
  similarity: number;
}

export interface SubflowComparison {
  subflowA: string;
  subflowB: string;
  blockDiffs: BlockComparison[];
  similarity: number;
}

export interface BlockComparison {
  blockA?: Block;
  blockB?: Block;
  change: string;
  similarity?: number;
}

export interface Highlight {
  start: number;
  end: number;
}

export interface SearchResult {
  blockId: string;
  subflowId: string;
  matchedField: string;
  matchedText: string;
  score: number;
  highlights: Highlight[];
}

export interface SearchResults {
  query: SearchQuery;
  results: SearchResult[];
  totalCount: number;
  durationMs: number;
}

export interface SearchQuery {
  text: string;
  blockTypes?: BlockType[];
  fuzzy: boolean;
  maxResults: number;
}

export interface ChatMessage {
  id: string;
  role: 'user' | 'assistant' | 'system';
  content: string;
  timestamp: string;
  contextBlockId?: string;
  contextSubflowId?: string;
  tokensIn?: number;
  tokensOut?: number;
  provider?: ProviderID;
  model?: string;
  finishReason?: 'stop' | 'interrupted' | 'error';
}

export interface ChatRequest {
  flowId: string;
  provider: string;
  model?: string;
  messages: ChatMessage[];
  userMessage: string;
  contextBlockId?: string;
  selectedSourceFiles?: string[];
  systemPrompt?: string;
  temperature?: number;
  maxTokens?: number;
  demoMode?: boolean;
  excludeContext?: boolean;
}

export interface SourceFileInfo {
  filename: string;
  subflowId: string;
  subflowName: string;
  blockCount: number;
  lineCount: number;
}

export interface ModelDetail {
  id: string;
  displayName: string;
  contextLimit: number;
  inputCostPerM: number;
  outputCostPerM: number;
}

export interface ProviderInfo {
  id: ProviderID;
  name: string;
  authType: AuthType;
  contextLimit: number;
  models: ModelDetail[];
  defaultModel: string;
  configured: boolean;
}

export interface AppSettings {
  version: number;
  general: GeneralSettings;
  appearance: AppearanceSettings;
  layout: LayoutSettings;
  ai: AISettings;
  parser: ParserSettings;
  analysis: AnalysisRulesSettings;
}

export interface GeneralSettings {
  firstRunCompleted: boolean;
  lastUsedVersion: string;
  checkForUpdates: 'never' | 'daily' | 'weekly' | 'monthly';
  openInNewWindow: boolean;
}

export interface AppearanceSettings {
  theme: ThemeMode;
  density: 'comfortable' | 'compact';
  codeFont: string;
  uiFont: string;
  reduceMotion: boolean;
  highContrast: boolean;
}

export interface LayoutSettings {
  sidebarWidth: number;
  inspectorWidth: number;
  sidebarCollapsed: boolean;
  inspectorCollapsed: boolean;
  lastActiveInspectorTab: 'details' | 'ai' | 'findings' | 'metrics' | 'sharing';
  lastViewMode: 'block' | 'graph' | 'map' | 'local-map' | 'diff' | 'profile' | 'admin';
  chatPanelHeight?: number;
}

export interface AIPromptsConfig {
  block: string[];
  flow: string[];
  finding: string[];
  blockWithFindings: string[];
}

export interface AISettings {
  activeProvider: ProviderID;
  providers: Record<ProviderID, AIProviderConfig>;
  demoMode: DemoModeSettings;
  showCostEstimates: boolean;
  saveConversationHistory: boolean;
  systemPromptSuffix?: string;
  dailyBudget: number;
  prompts: AIPromptsConfig;
}

export interface AIProviderConfig {
  enabled: boolean;
  defaultModel: string;
  temperature: number;
  maxTokens: number;
  contextTokenBudget: number;
}

export interface DemoModeSettings {
  enabled: boolean;
  dailyLimit: number;
  dailyUsed: number;
  resetDate: string;
}

export interface ParserSettings {
  maxFileSizeMB: number;
  preserveComments: boolean;
  treatTabsAsSpaces: boolean;
  spacesPerIndent: number;
}

export interface AnalysisRulesSettings {
  rules: Record<string, RuleConfig>;
  autoAnalyzeOnOpen: boolean;
}

export interface RuleConfig {
  enabled: boolean;
  severity: Severity;
  options?: Record<string, unknown>;
}

export interface Rule {
  id: string
  name: string
  description: string
  defaultSeverity: Severity
  category: string
  enabled: boolean
}

export interface ChatResponse {
  message: ChatMessage
  usage: TokenUsage
  durationMs: number
}

export interface TokenUsage {
  promptTokens: number
  completionTokens: number
  totalTokens: number
}

export interface ConversationSummary {
  id: string
  flowId: string
  provider: ProviderID
  model: string
  messageCount: number
  createdAt: string
  lastMessageAt: string
}

export interface ContextPreview {
  systemPrompt: string;
  contextText: string;
  userMessage: string;
  estimatedTokens: number;
  contextLimit: number;
}

export interface AppInfo {
  version: string;
  platform: string;
  arch: string;
  buildDate: string;
  gitCommit: string;
}

export interface ConversationFile {
  version: number;
  flowKey: string;
  scope: string;
  updatedAt: string;
  messages: ChatMessage[];
}

export interface ProviderTestResult {
  ok: boolean;
  latencyMs: number;
  error?: string;
}

export interface DeviceAuthResponse {
  device_code: string;
  user_code: string;
  verification_uri: string;
  expires_in: number;
  interval: number;
}

export interface GitHubAuthResult {
  status: string;
  token?: string;
  error?: string;
}

export interface GitHubUser {
  login: string;
  name: string;
}
