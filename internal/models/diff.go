package models

type FlowDiff struct {
	OldID    string        `json:"oldId"`
	NewID    string        `json:"newId"`
	Subflows []SubflowDiff `json:"subflows"`
}

type SubflowDiff struct {
	Name   string     `json:"name"`
	Change ChangeType `json:"change"`
	Blocks []BlockDiff `json:"blocks"`
}

type BlockDiff struct {
	Change ChangeType `json:"change"`
	
	// For Modified, we might have both. For Added only New. For Removed only Old.
	Old *Block `json:"old,omitempty"`
	New *Block `json:"new,omitempty"`
}
