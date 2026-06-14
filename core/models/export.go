package models

type ExportOptions struct {
	IncludeFindings bool   `json:"includeFindings"`
	IncludeChats    bool   `json:"includeChats"`
	SeverityFilter  string `json:"severityFilter"`
	Format          string `json:"format"`
}
