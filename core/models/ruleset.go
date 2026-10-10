package models

// RuleRef identifies one rule as it ran. Rev is "" for a built-in at its first
// revision, "r<N>" at revision N > 1, and a custom rule's match digest; Stage
// is "preview" or "" (stable). A change in any of the three makes the rule new
// relative to a recorded RuleSet. Persisted: never rename the JSON keys.
type RuleRef struct {
	ID    string `json:"id"`
	Rev   string `json:"rev,omitempty"`
	Stage string `json:"stage,omitempty"`
}

// RuleSet records which rules produced an analysis, so a baseline or health
// snapshot can later tell findings of rules it never saw from drift
// (analyzer.NewRuleAge). Rules is sorted by (ID, Rev, Stage) without
// duplicates; Digest identifies the set ("rs1:" + hex SHA-256, see
// analyzer.NewRuleSet) and is persisted.
type RuleSet struct {
	Rules  []RuleRef `json:"rules"`
	Digest string    `json:"digest"`
}
