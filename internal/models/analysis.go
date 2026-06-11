package models

import "time"

type Severity string

const (
	SeverityError   Severity = "error"
	SeverityWarning Severity = "warning"
	SeverityInfo    Severity = "info"
)

type Finding struct {
	ID          string                 `json:"id"`
	RuleID      string                 `json:"ruleId"`
	Severity    Severity               `json:"severity"`
	Title       string                 `json:"title"`
	Description string                 `json:"description"`
	BlockID     string                 `json:"blockId"`
	SubflowID   string                 `json:"subflowId"`
	Suggestion  string                 `json:"suggestion,omitempty"`
	AutoFixHint string                 `json:"autoFixHint,omitempty"`
	Category    string                 `json:"category,omitempty"`
	Metadata    map[string]interface{} `json:"metadata,omitempty"`
}

type AnalysisReport struct {
	FlowID        string         `json:"flowId"`
	FlowName      string         `json:"flowName,omitempty"`
	GeneratedAt   time.Time      `json:"generatedAt"`
	Findings      []Finding      `json:"findings"`
	Stats         AnalysisStats  `json:"stats"`
	DurationMs    int            `json:"durationMs"`
	Metrics       *FlowMetrics   `json:"metrics,omitempty"`
	RuleProfiles  []RuleProfile  `json:"ruleProfiles,omitempty"`
}

type AnalysisStats struct {
	Errors         int `json:"errors"`
	Warnings       int `json:"warnings"`
	Info           int `json:"info"`
	BlocksAnalyzed int `json:"blocksAnalyzed"`
	RulesRun       int `json:"rulesRun"`
}

type RuleProfile struct {
	RuleID      string `json:"ruleId"`
	RuleName    string `json:"ruleName"`
	DurationMs  int64  `json:"durationMs"`
	FindingCount int   `json:"findingCount"`
	BlocksChecked int  `json:"blocksChecked"`
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
	FlowID   string              `json:"flowId"`
	FlowName string              `json:"flowName"`
	Report   *AnalysisReport     `json:"report"`
	Error    string              `json:"error,omitempty"`
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
	FlowID         string   `json:"flowId"`
	Added          []Finding `json:"added"`
	Removed        []Finding `json:"removed"`
	Persisted      []Finding `json:"persisted"`
	AddedCount     int      `json:"addedCount"`
	RemovedCount   int      `json:"removedCount"`
	PersistedCount int      `json:"persistedCount"`
	// HasPrevious is false when no earlier analysis run exists to compare
	// against (the diff is then "everything added" by construction).
	HasPrevious    bool     `json:"hasPrevious"`
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
	TotalFlowsAnalyzed int                `json:"totalFlowsAnalyzed"`
	TotalFindings      int                `json:"totalFindings"`
	FindingsBySeverity map[string]int     `json:"findingsBySeverity"`
	FindingsByCategory map[string]int     `json:"findingsByCategory"`
	FindingsByRule     map[string]int     `json:"findingsByRule"`
	AvgHealthScore     float64            `json:"avgHealthScore"`
	TopProblemFlows    []ProblemFlow      `json:"topProblemFlows"`
}

type ProblemFlow struct {
	FlowID       string `json:"flowId"`
	FlowName     string `json:"flowName"`
	FindingCount int    `json:"findingCount"`
	HealthScore  int    `json:"healthScore"`
}

type FindingGroup struct {
	BlockID        string   `json:"blockId"`
	Findings       []Finding `json:"findings"`
	Primary        *Finding  `json:"primary"`
	DuplicateCount int      `json:"duplicateCount"`
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
