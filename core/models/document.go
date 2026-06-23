package models

import "time"

type ChangeType string

const (
	ChangeNone     ChangeType = "none"
	ChangeAdded    ChangeType = "added"
	ChangeRemoved  ChangeType = "removed"
	ChangeModified ChangeType = "modified"
)

type FlowDocument struct {
	ID             string         `json:"id"`
	Name           string         `json:"name"`
	FilePath       string         `json:"filePath"`
	OwnerID        string         `json:"ownerId,omitempty"`
	OrganizationID string         `json:"orgId,omitempty"`
	Subflows       []Subflow      `json:"subflows"`
	ParseErrors    []ParseError   `json:"parseErrors,omitempty"`
	Metadata       FlowMetadata   `json:"metadata"`
	Files          []FlowFileInfo `json:"files,omitempty"`
	IsFolder       bool           `json:"isFolder,omitempty"`

	// Transient lookup maps (not serialized to JSON)
	BlocksByID   map[string]*Block   `json:"-"`
	BlockSubflow map[string]*Subflow `json:"-"`
	SubflowsByID map[string]*Subflow `json:"-"`
}

// RebuildIndexes repopulates the transient lookup maps (BlocksByID,
// BlockSubflow, SubflowsByID) by walking Subflows and their (possibly nested)
// blocks. These maps are not serialized (json:"-"), so this MUST be called
// after deserializing a FlowDocument from stored JSON before the document is
// used for analysis/lineage/graph. It mirrors the indexing the parser performs
// at parse time.
func (d *FlowDocument) RebuildIndexes() {
	d.BlocksByID = make(map[string]*Block)
	d.BlockSubflow = make(map[string]*Subflow)
	d.SubflowsByID = make(map[string]*Subflow)
	for i := range d.Subflows {
		sf := &d.Subflows[i]
		d.SubflowsByID[sf.ID] = sf
		for j := range sf.Blocks {
			d.indexBlock(sf, &sf.Blocks[j])
		}
	}
}

func (d *FlowDocument) indexBlock(sf *Subflow, b *Block) {
	d.BlocksByID[b.ID] = b
	d.BlockSubflow[b.ID] = sf
	for i := range b.Children {
		d.indexBlock(sf, &b.Children[i])
	}
}

type FlowMetadata struct {
	BlockCount   int       `json:"blockCount"`
	SubflowCount int       `json:"subflowCount"`
	MaxDepth     int       `json:"maxDepth"`
	ParsedAt     time.Time `json:"parsedAt"`
	FileSize     int64     `json:"fileSize"`
	RawLineCount int       `json:"rawLineCount"`
}

type ParseError struct {
	Line     int    `json:"line"`
	Column   int    `json:"column,omitempty"`
	Message  string `json:"message"`
	Severity string `json:"severity"`
	Snippet  string `json:"snippet"`
}

type RecentFile struct {
	Path     string    `json:"path"`
	Name     string    `json:"name"`
	Size     int64     `json:"size"`
	LastOpen time.Time `json:"lastOpen"`
	IsFolder bool      `json:"isFolder"`
}

type FlowFileInfo struct {
	Path string `json:"path"`
	Name string `json:"name"`
	Size int64  `json:"size"`
}
