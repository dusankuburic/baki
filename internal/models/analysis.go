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
	Metadata    map[string]interface{} `json:"metadata,omitempty"`
}

type AnalysisReport struct {
	FlowID      string         `json:"flowId"`
	GeneratedAt time.Time      `json:"generatedAt"`
	Findings    []Finding      `json:"findings"`
	Stats       AnalysisStats  `json:"stats"`
	DurationMs  int            `json:"durationMs"`
}

type AnalysisStats struct {
	Errors         int `json:"errors"`
	Warnings       int `json:"warnings"`
	Info           int `json:"info"`
	BlocksAnalyzed int `json:"blocksAnalyzed"`
	RulesRun       int `json:"rulesRun"`
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
