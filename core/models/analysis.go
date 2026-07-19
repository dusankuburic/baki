package models

import "time"

type Severity string

const (
	SeverityError   Severity = "error"
	SeverityWarning Severity = "warning"
	SeverityInfo    Severity = "info"
)

// Confidence expresses how sure a rule is about a given finding — separating
// "we're sure" (regex secret match, an open without a close) from "maybe"
// (Shannon-entropy guess, name-heuristic uninitialized variable). Findings sort
// by severity × confidence so a triager hits the sure-fire issues first, and a
// low-confidence finding can carry a "maybe" affordance in the UI. Defaults to
// Medium when a rule doesn't say.
type Confidence string

const (
	ConfidenceHigh   Confidence = "high"
	ConfidenceMedium Confidence = "medium"
	ConfidenceLow    Confidence = "low"
)

type Finding struct {
	ID string `json:"id"`
	// Fingerprint is a stable, content-derived identity for the finding (see
	// ContentKey in the analyzer). Unlike ID — a per-run sequential index
	// ("F-001") that shifts when other findings come or go — Fingerprint
	// survives re-analysis AND re-imports/re-parses, so triage state,
	// suppressions, baselines, and SARIF can be pinned to it.
	Fingerprint string   `json:"fingerprint,omitempty"`
	RuleID      string   `json:"ruleId"`
	Severity    Severity `json:"severity"`
	// Confidence is the rule's certainty about this finding (high/medium/low).
	// Stamped by the engine (per-rule default, overridable by the rule). Drives
	// severity×confidence triage ordering.
	Confidence  Confidence `json:"confidence,omitempty"`
	Title       string     `json:"title"`
	Description string     `json:"description"`
	BlockID     string     `json:"blockId"`
	SubflowID   string     `json:"subflowId"`
	Suggestion  string     `json:"suggestion,omitempty"`
	AutoFixHint string     `json:"autoFixHint,omitempty"`
	// AutoFix names a deterministic fix the user can apply in one click from the
	// findings UI (desktop: edits the source file, re-parses, re-analyzes).
	// Empty means no automatic fix is available (only the AutoFixHint prose or
	// "Fix with AI"). Current values: "wrap-error-handler" (resolves
	// unhandled-error / file-op-no-error-handler), "suppress" (pad-ignore).
	AutoFix  string                 `json:"autoFix,omitempty"`
	Category string                 `json:"category,omitempty"`
	Metadata map[string]interface{} `json:"metadata,omitempty"`
}

// Key returns the in-run identity of a finding: the rule that raised it plus the
// block it points at. Block IDs are flow-unique GUIDs, so RuleID:BlockID
// uniquely identifies "this rule firing on this block" WITHIN one parsed run.
//
// For cross-run identity (diff, baseline, triage, SARIF), use Fingerprint
// instead — it is content-derived (rule + subflow/name/line/subject) and
// survives re-imports and CLI re-runs, whereas Key() changes every re-parse.
//
// Stability note: block IDs are assigned by the parser (uuid.NewString) at parse
// time, so a Key is stable only across re-analyses of the *same parsed
// document* (the cloud server stores the parsed doc and re-analyzes it in
// place) but NOT across independent re-parses of the source text. Fingerprint
// covers the cross-parse case.
func (f Finding) Key() string {
	return f.RuleID + ":" + f.BlockID
}

type AnalysisReport struct {
	FlowID      string    `json:"flowId"`
	FlowName    string    `json:"flowName,omitempty"`
	GeneratedAt time.Time `json:"generatedAt"`
	Findings    []Finding `json:"findings"`
	// Groups are the per-block finding clusters produced by DeduplicateFindings
	// (run in the default analysis path). Each group's Primary is the
	// representative finding; DuplicateCount is how many same-block, same-subject
	// duplicates were folded into it. Drives the UI's "N similar" affordance.
	Groups       []FindingGroup `json:"groups,omitempty"`
	Stats        AnalysisStats  `json:"stats"`
	DurationMs   int            `json:"durationMs"`
	Metrics      *FlowMetrics   `json:"metrics,omitempty"`
	RuleProfiles []RuleProfile  `json:"ruleProfiles,omitempty"`
}

type AnalysisStats struct {
	Errors         int `json:"errors"`
	Warnings       int `json:"warnings"`
	Info           int `json:"info"`
	BlocksAnalyzed int `json:"blocksAnalyzed"`
	RulesRun       int `json:"rulesRun"`
	// RulesSkipped counts (block, rule) evaluations that were aborted via
	// safeCheck's panic recovery — one buggy rule or malformed block can't
	// crash the run, but operators need to see that findings may be missing.
	RulesSkipped int `json:"rulesSkipped"`
	// Suppressed counts findings hidden by inline `# pad-ignore` directives in
	// the flow source (see analyzer suppression). They are excluded from
	// Errors/Warnings/Info and from the findings list.
	Suppressed int `json:"suppressed"`
}

type RuleProfile struct {
	RuleID        string `json:"ruleId"`
	RuleName      string `json:"ruleName"`
	DurationMs    int64  `json:"durationMs"`
	FindingCount  int    `json:"findingCount"`
	BlocksChecked int    `json:"blocksChecked"`
}

type SubflowMetrics struct {
	SubflowID            string `json:"subflowId"`
	SubflowName          string `json:"subflowName"`
	BlockCount           int    `json:"blockCount"`
	CyclomaticComplexity int    `json:"cyclomaticComplexity"`
	CognitiveComplexity  int    `json:"cognitiveComplexity"`
	MaxNestingDepth      int    `json:"maxNestingDepth"`
	VariableCount        int    `json:"variableCount"`
	FanIn                int    `json:"fanIn"`
	FanOut               int    `json:"fanOut"`
}

type FlowMetrics struct {
	Subflows             []SubflowMetrics `json:"subflows"`
	TotalBlocks          int              `json:"totalBlocks"`
	TotalVariables       int              `json:"totalVariables"`
	MaxCyclomatic        int              `json:"maxCyclomatic"`
	AvgCyclomatic        float64          `json:"avgCyclomatic"`
	MaxCognitive         int              `json:"maxCognitive"`
	AvgCognitive         float64          `json:"avgCognitive"`
	HealthScore          int              `json:"healthScore"`
	VariableDensity      float64          `json:"variableDensity"`
	SubflowCount         int              `json:"subflowCount"`
	CircularDependencies []string         `json:"circularDependencies,omitempty"`
}

type Rule struct {
	ID              string   `json:"id"`
	Name            string   `json:"name"`
	Description     string   `json:"description"`
	DefaultSeverity Severity `json:"defaultSeverity"`
	Category        string   `json:"category"`
	Enabled         bool     `json:"enabled"`
	// Confidence is the rule's built-in default certainty (high/medium/low),
	// surfaced at the catalog level so the UI can show which results to trust
	// without running an analysis. A rule may still raise/lower it per finding.
	Confidence Confidence `json:"confidence"`
	// AutoFix is the deterministic fixType (e.g. "set-timeout") a user can
	// apply in one click, or "" when the rule has no auto-fixer. Lets the
	// dashboard compute "auto-fixable rule" counts.
	AutoFix string `json:"autoFix,omitempty"`
}

// RuleSummary is the catalog-level rollup the dashboard consumes for its
// "auto-fixable rules" and "confidence distribution" KPIs. It folds the full
// rule catalog into counts so the client doesn't have to iterate every rule.
type RuleSummary struct {
	TotalRules       int            `json:"totalRules"`
	AutoFixableRules int            `json:"autoFixableRules"`
	ByCategory       map[string]int `json:"byCategory"`
	ByConfidence     map[string]int `json:"byConfidence"`
}

type VariableEvent struct {
	Type      string `json:"type"` // "init", "mutate", "read"
	BlockID   string `json:"blockId"`
	Line      int    `json:"line"`
	SubflowID string `json:"subflowId"`
}

type VariableHistory struct {
	Name   string          `json:"name"`
	Events []VariableEvent `json:"events"`
}

type BlockDataFlow struct {
	BlockID          string   `json:"blockId"`
	SubflowID        string   `json:"subflowId"`
	Reads            []string `json:"reads"`
	Writes           []string `json:"writes"`
	UpstreamBlocks   []string `json:"upstreamBlocks"`
	DownstreamBlocks []string `json:"downstreamBlocks"`
}

type TaintPath struct {
	SourceVar   string   `json:"sourceVar"`
	SourceBlock string   `json:"sourceBlock"`
	SinkBlock   string   `json:"sinkBlock"`
	SinkType    string   `json:"sinkType"`
	Path        []string `json:"path"`
}

type DeadDataPath struct {
	Variable  string `json:"variable"`
	SetBlock  string `json:"setBlock"`
	ReadBlock string `json:"readBlock"`
	Reason    string `json:"reason"`
}

type DataFlowAnalysis struct {
	Blocks     map[string]*BlockDataFlow `json:"blocks"`
	TaintPaths []TaintPath               `json:"taintPaths"`
	DeadData   []DeadDataPath            `json:"deadData"`
}

type BatchResult struct {
	FlowID   string          `json:"flowId"`
	FlowName string          `json:"flowName"`
	Report   *AnalysisReport `json:"report"`
	Error    string          `json:"error,omitempty"`
}

type BatchAnalysis struct {
	Results        []BatchResult `json:"results"`
	TotalFlows     int           `json:"totalFlows"`
	TotalFindings  int           `json:"totalFindings"`
	TotalErrors    int           `json:"totalErrors"`
	TotalWarnings  int           `json:"totalWarnings"`
	TotalInfo      int           `json:"totalInfo"`
	AvgHealthScore float64       `json:"avgHealthScore"`
	DurationMs     int           `json:"durationMs"`
}

type AnalysisDiff struct {
	FlowID         string    `json:"flowId"`
	Added          []Finding `json:"added"`
	Removed        []Finding `json:"removed"`
	Persisted      []Finding `json:"persisted"`
	AddedCount     int       `json:"addedCount"`
	RemovedCount   int       `json:"removedCount"`
	PersistedCount int       `json:"persistedCount"`
	// HasPrevious is false when no earlier analysis run exists to compare
	// against (the diff is then "everything added" by construction).
	HasPrevious bool `json:"hasPrevious"`
}

// BaselineDrift reports the findings in a report that are NOT in a flow's
// accepted baseline — i.e. findings introduced since the baseline was taken.
// It is the basis for ratcheting/gating: CI and dashboards can fail or alert on
// New (and especially NewErrors) while ignoring already-accepted findings.
// When HasBaseline is false (no baseline recorded), every finding is "new" by
// construction, mirroring AnalysisDiff's "no prior run ⇒ everything added".
type BaselineDrift struct {
	FlowID      string    `json:"flowId"`
	HasBaseline bool      `json:"hasBaseline"`
	New         []Finding `json:"new"`
	NewErrors   int       `json:"newErrors"`
	NewWarnings int       `json:"newWarnings"`
	NewInfo     int       `json:"newInfo"`
}

// PortfolioEntry is one flow in the org-wide governance portfolio: its latest
// persisted health and finding counts, used to rank flows by risk.
type PortfolioEntry struct {
	FlowID      string     `json:"flowId"`
	FlowName    string     `json:"flowName"`
	OwnerID     string     `json:"ownerId,omitempty"`
	OwnerName   string     `json:"ownerName,omitempty"`
	Analyzed    bool       `json:"analyzed"`
	HealthScore int        `json:"healthScore"`
	Errors      int        `json:"errors"`
	Warnings    int        `json:"warnings"`
	Info        int        `json:"info"`
	AnalyzedAt  *time.Time `json:"analyzedAt,omitempty"`
}

// Portfolio is the fleet view: every flow the caller can govern, ranked worst-
// health-first, with rollup totals. Unanalyzed flows sort last (no health yet).
type Portfolio struct {
	Entries       []PortfolioEntry `json:"entries"`
	TotalFlows    int              `json:"totalFlows"`
	AnalyzedFlows int              `json:"analyzedFlows"`
	AvgHealth     int              `json:"avgHealth"`
	Errors        int              `json:"errors"`
	Warnings      int              `json:"warnings"`
	Info          int              `json:"info"`
}

type GraphNode struct {
	ID         string `json:"id"`
	Label      string `json:"label"`
	Type       string `json:"type"` // "subflow"
	BlockCount int    `json:"blockCount"`
	ErrorCount int    `json:"errorCount"`
	WarnCount  int    `json:"warnCount"`
}

type GraphEdge struct {
	Source string `json:"source"`
	Target string `json:"target"`
}

type GraphData struct {
	Nodes []GraphNode `json:"nodes"`
	Edges []GraphEdge `json:"edges"`
}

type RuleDependency struct {
	FromRuleID string `json:"fromRuleId"`
	ToRuleID   string `json:"toRuleId"`
	Reason     string `json:"reason"`
}

type DependencyAnalysis struct {
	Dependencies []RuleDependency `json:"dependencies"`
	Cycles       [][]string       `json:"cycles"`
	TopoOrder    []string         `json:"topoOrder"`
}

type SubflowHash struct {
	SubflowID string `json:"subflowId"`
	Hash      string `json:"hash"`
}

type DashboardStats struct {
	TotalFlowsAnalyzed int            `json:"totalFlowsAnalyzed"`
	TotalSubflows      int            `json:"totalSubflows"`
	TotalFindings      int            `json:"totalFindings"`
	FindingsBySeverity map[string]int `json:"findingsBySeverity"`
	FindingsByCategory map[string]int `json:"findingsByCategory"`
	FindingsByRule     map[string]int `json:"findingsByRule"`
	AvgHealthScore     float64        `json:"avgHealthScore"`
	TopProblemFlows    []ProblemFlow  `json:"topProblemFlows"`
}

type ProblemFlow struct {
	FlowID       string `json:"flowId"`
	FlowName     string `json:"flowName"`
	FindingCount int    `json:"findingCount"`
	HealthScore  int    `json:"healthScore"`
}

type FindingGroup struct {
	BlockID        string    `json:"blockId"`
	Findings       []Finding `json:"findings"`
	Primary        *Finding  `json:"primary"`
	DuplicateCount int       `json:"duplicateCount"`
}

type FlowComparison struct {
	FlowAID       string              `json:"flowAId"`
	FlowBID       string              `json:"flowBId"`
	SubflowDiff   []SubflowComparison `json:"subflowDiff"`
	SharedBlocks  int                 `json:"sharedBlocks"`
	AddedBlocks   int                 `json:"addedBlocks"`
	RemovedBlocks int                 `json:"removedBlocks"`
	Similarity    float64             `json:"similarity"`
}

type SubflowComparison struct {
	SubflowA   string            `json:"subflowA"`
	SubflowB   string            `json:"subflowB"`
	BlockDiffs []BlockComparison `json:"blockDiffs"`
	Similarity float64           `json:"similarity"`
}

type BlockComparison struct {
	BlockA     *Block  `json:"blockA,omitempty"`
	BlockB     *Block  `json:"blockB,omitempty"`
	Change     string  `json:"change"`
	Similarity float64 `json:"similarity,omitempty"`
}
