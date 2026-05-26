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
	ID          string        `json:"id"`
	Name        string        `json:"name"`
	FilePath    string        `json:"filePath"`
	Subflows    []Subflow     `json:"subflows"`
	ParseErrors []ParseError  `json:"parseErrors,omitempty"`
	Metadata    FlowMetadata  `json:"metadata"`
	Files       []FlowFileInfo `json:"files,omitempty"`
	IsFolder    bool          `json:"isFolder,omitempty"`

	// Transient lookup maps (not serialized to JSON)
	BlocksByID   map[string]*Block   `json:"-"`
	BlockSubflow map[string]*Subflow `json:"-"`
	SubflowsByID map[string]*Subflow `json:"-"`
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
