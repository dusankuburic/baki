package models

type SearchQuery struct {
	Text       string      `json:"text"`
	BlockTypes []BlockType `json:"blockTypes,omitempty"`
	Fuzzy      bool        `json:"fuzzy"`
	MaxResults int         `json:"maxResults"`
}

type SearchResult struct {
	BlockID      string      `json:"blockId"`
	SubflowID    string      `json:"subflowId"`
	MatchedField string      `json:"matchedField"`
	MatchedText  string      `json:"matchedText"`
	Score        int         `json:"score"`
	Highlights   []Highlight `json:"highlights"`
}

type Highlight struct {
	Start int `json:"start"`
	End   int `json:"end"`
}

type SearchResults struct {
	Query      SearchQuery    `json:"query"`
	Results    []SearchResult `json:"results"`
	TotalCount int            `json:"totalCount"`
	DurationMs int            `json:"durationMs"`
}
