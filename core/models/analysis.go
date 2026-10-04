package models

import "time"

type Severity string

const (
	SeverityError   Severity = "error"
	SeverityWarning Severity = "warning"
	SeverityInfo    Severity = "info"
)

// Valid reports whether s is one of the three severities every consumer
// switches or ranks over; anything else is silently dropped (a custom
// "critical" once vanished and turned CI green). Severity round-trips JSON as
// a plain string, so validate at the boundary.
func (s Severity) Valid() bool {
	switch s {
	case SeverityError, SeverityWarning, SeverityInfo:
		return true
	}
	return false
}

// ValidSeverities lists the accepted values, for error messages.
func ValidSeverities() []Severity {
	return []Severity{SeverityError, SeverityWarning, SeverityInfo}
}

// Confidence expresses how sure a rule is about a finding — "we're sure"
// (regex secret match) vs "maybe" (entropy guess, heuristic). Findings sort by
// severity × confidence. Defaults to Medium.
type Confidence string

const (
	ConfidenceHigh   Confidence = "high"
	ConfidenceMedium Confidence = "medium"
	ConfidenceLow    Confidence = "low"
)

type Finding struct {
	ID string `json:"id"`
	// Fingerprint is a stable, content-derived identity (analyzer.ContentKey):
	// unlike ID, it survives re-analysis AND re-imports/re-parses, so triage
	// state, suppressions, baselines and SARIF pin to it.
	Fingerprint string   `json:"fingerprint,omitempty"`
	RuleID      string   `json:"ruleId"`
	Severity    Severity `json:"severity"`
	// Confidence is the rule's certainty (engine-stamped default, overridable);
	// drives severity×confidence triage ordering.
	Confidence  Confidence `json:"confidence,omitempty"`
	Title       string     `json:"title"`
	Description string     `json:"description"`
	BlockID     string     `json:"blockId"`
	SubflowID   string     `json:"subflowId"`
	Suggestion  string     `json:"suggestion,omitempty"`
	AutoFixHint string     `json:"autoFixHint,omitempty"`
	// AutoFix names a deterministic one-click fix from the findings UI (edits
	// source, re-parses, re-analyzes); empty means only the hint or "Fix with AI".
	AutoFix  string                 `json:"autoFix,omitempty"`
	Category string                 `json:"category,omitempty"`
	Metadata map[string]interface{} `json:"metadata,omitempty"`
}

// Key returns the in-run identity RuleID:BlockID — stable across re-analyses
// of the same parsed document, NOT across re-parses (block IDs are
// parser-minted UUIDs); use Fingerprint cross-run. Pointer receiver: Finding
// is ~200 bytes.
func (f *Finding) Key() string {
	return f.RuleID + ":" + f.BlockID
}

type AnalysisReport struct {
	FlowID      string    `json:"flowId"`
	FlowName    string    `json:"flowName,omitempty"`
	GeneratedAt time.Time `json:"generatedAt"`
	Findings    []Finding `json:"findings"`
	// Per-block finding groups are deliberately NOT a field: they would repeat
	// each finding plus a Primary copy nobody reads; the dedup view computes
	// its own (analyzer.DeduplicateFindings).
	Stats AnalysisStats `json:"stats"`
	// ContentHash carries the analyzed document's FlowHash (set by the
	// cache-key path so history reuses it); not serialized.
	ContentHash  string        `json:"-"`
	DurationMs   int           `json:"durationMs"`
	Metrics      *FlowMetrics  `json:"metrics,omitempty"`
	RuleProfiles []RuleProfile `json:"ruleProfiles,omitempty"`
}

type AnalysisStats struct {
	Errors         int `json:"errors"`
	Warnings       int `json:"warnings"`
	Info           int `json:"info"`
	BlocksAnalyzed int `json:"blocksAnalyzed"`
	RulesRun       int `json:"rulesRun"`
	// RulesSkipped counts (block, rule) evaluations aborted via safeCheck's
	// panic recovery — one buggy rule can't crash the run, but findings may be missing.
	RulesSkipped int `json:"rulesSkipped"`
	// Suppressed counts findings hidden by inline `# pad-ignore` directives;
	// excluded from Errors/Warnings/Info and from the findings list.
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
	// Confidence is the built-in default certainty, surfaced at catalog level
	// so the UI can show which results to trust; still overridable per finding.
	Confidence Confidence `json:"confidence"`
	// AutoFix is the deterministic fixType (e.g. "set-timeout") a user can
	// apply in one click, or "" when the rule has no auto-fixer.
	AutoFix string `json:"autoFix,omitempty"`
	// Options are the rule's numeric thresholds (maxDepth, minRepeats,
	// maxBlocks) — deployment override when one exists, else built-in default.
	Options map[string]interface{} `json:"options,omitempty"`
}

// RuleSummary is the catalog-level rollup for the dashboard's "auto-fixable
// rules" and "confidence distribution" KPIs, folding the catalog into counts.
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

// BaselineDrift reports findings NOT in a flow's accepted baseline — the
// ratcheting/gating basis: CI and dashboards fail or alert on New (especially
// NewErrors) while ignoring accepted findings. Without a baseline every
// finding is "new".
type BaselineDrift struct {
	FlowID      string    `json:"flowId"`
	HasBaseline bool      `json:"hasBaseline"`
	New         []Finding `json:"new"`
	NewErrors   int       `json:"newErrors"`
	NewWarnings int       `json:"newWarnings"`
	NewInfo     int       `json:"newInfo"`
}

// PortfolioEntry is one flow in the org-wide governance portfolio: latest
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
	Entries []PortfolioEntry `json:"entries"`
	// TotalFlows is every flow under the requested scope; RankedFlows is how
	// many this response actually ranked (in-memory, capped) — the rollups
	// below cover exactly the ranked flows; Truncated marks the cap.
	TotalFlows    int  `json:"totalFlows"`
	RankedFlows   int  `json:"rankedFlows"`
	Truncated     bool `json:"truncated"`
	AnalyzedFlows int  `json:"analyzedFlows"`
	AvgHealth     int  `json:"avgHealth"`
	Errors        int  `json:"errors"`
	Warnings      int  `json:"warnings"`
	Info          int  `json:"info"`
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

// DependencyAnalysis is the payload of GET /api/analysis/dependencies: the
// analyzer's hand-maintained "related rules" map (see analyzer.relatedRules).
type DependencyAnalysis struct {
	Dependencies []RuleDependency `json:"dependencies"`
	Cycles       [][]string       `json:"cycles"`
	TopoOrder    []string         `json:"topoOrder"`
	// Editorial marks the map as hand-maintained: it is not derived from rule
	// behaviour and has no effect on evaluation order or results.
	Editorial bool `json:"editorial"`
	// Description explains the map's provenance to whoever renders it.
	Description string `json:"description,omitempty"`
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
