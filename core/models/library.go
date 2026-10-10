package models

import (
	"encoding/json"
	"time"
)

// LibraryFacts is one library rule's extract of one flow, at the rule's
// FactsVersion. Facts at another version are stale and never reach the rule.
type LibraryFacts struct {
	Version int             `json:"version"`
	Data    json.RawMessage `json:"data"`
}

// FlowSummary is what a library rule knows about one flow: the bounded facts
// each library rule extracted from it (analyzer.ExtractFlowSummary). The
// library phase never sees the document itself.
type FlowSummary struct {
	// FlowID is the caller's identity of the flow (the document ID; a stored
	// flow's ID in the cloud). Library findings name their flow by it.
	FlowID string `json:"flowId"`
	// StableID survives re-parsing (analyzer.StableFlowID); fingerprints use it.
	StableID    string `json:"stableId,omitempty"`
	FlowName    string `json:"flowName,omitempty"`
	ContentHash string `json:"contentHash,omitempty"`
	// Facts by library rule ID; a rule with nothing to say about the flow has no entry.
	Facts map[string]LibraryFacts `json:"facts,omitempty"`
	// Skipped lists rules whose extraction failed (error, panic, output over
	// the cap); they see this flow without facts.
	Skipped []string `json:"skipped,omitempty"`
}

// LibraryLocation points at a subflow (or, with no subflow name, a whole flow)
// in the library.
type LibraryLocation struct {
	FlowID      string `json:"flowId"`
	FlowName    string `json:"flowName,omitempty"`
	SubflowName string `json:"subflowName,omitempty"`
}

// LibraryFinding is a finding about one flow that only shows across flows. It
// never enters an AnalysisReport, the report caches, baselines, drift or gates.
type LibraryFinding struct {
	Finding
	FlowID      string `json:"flowId"`
	FlowName    string `json:"flowName,omitempty"`
	SubflowName string `json:"subflowName,omitempty"`
	// Related are the other places involved, sorted and capped.
	Related []LibraryLocation `json:"related,omitempty"`
	// Anchor is a content-derived key of what the finding points at inside its
	// flow beyond the subflow name (never a parser-minted ID); the fingerprint
	// covers the flow's StableID, SubflowName and Anchor.
	Anchor string `json:"-"`
}

// LibraryReport is the result of one library pass over a set of flows.
type LibraryReport struct {
	Findings []LibraryFinding `json:"findings"`
	// Flows is how many flows the rules saw; Truncated marks a flow or
	// findings cap, so some flows or findings were left out.
	Flows     int  `json:"flows"`
	Truncated bool `json:"truncated,omitempty"`
	RulesRun  int  `json:"rulesRun"`
	// RulesSkipped counts rules that panicked, failed or were cancelled.
	RulesSkipped int       `json:"rulesSkipped,omitempty"`
	GeneratedAt  time.Time `json:"generatedAt"`
	DurationMs   int       `json:"durationMs"`
}
