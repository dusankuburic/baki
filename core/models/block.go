package models

type BlockType string

const (
	BlockTypeAction       BlockType = "ACTION"
	BlockTypeLoop         BlockType = "LOOP"
	BlockTypeCondition    BlockType = "CONDITION"
	BlockTypeSubflow      BlockType = "SUBFLOW"
	BlockTypeErrorHandler BlockType = "ERROR_HANDLER"
	BlockTypeComment      BlockType = "COMMENT"
	BlockTypeVariable     BlockType = "VARIABLE"
	BlockTypeWait         BlockType = "WAIT"
	BlockTypeElse         BlockType = "ELSE"
	BlockTypeCase         BlockType = "CASE"
	BlockTypeDefault      BlockType = "DEFAULT"
	BlockTypeBlock        BlockType = "BLOCK"
	BlockTypeSwitch       BlockType = "SWITCH"
	BlockTypeEnd          BlockType = "END"
	BlockTypeUnknown      BlockType = "UNKNOWN"
)

type BlockToken struct {
	Type   string `json:"type"`             // "text", "variable", "subflow", "label"
	Value  string `json:"value"`            // The text to display
	Target string `json:"target,omitempty"` // The ID or name of the target (subflow name, variable name, etc.)
}

type Block struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	Type       BlockType `json:"type"`
	RawType    string    `json:"rawType"`
	Indent     int       `json:"indent"`
	LineNumber int       `json:"lineNumber"`
	// EndLineNumber is the last physical line the block spans (0 = same as
	// LineNumber). Non-zero only for leaf actions whose value is a multi-line
	// triple-quoted literal. Autofixers consult it via blockEndLine to place
	// append/wrap/remove patches after the literal rather than inside it.
	EndLineNumber int               `json:"endLineNumber,omitempty"`
	Children      []Block           `json:"children"`
	Properties    map[string]string `json:"properties"`
	Variables     []string          `json:"variables"`
	Tokens        []BlockToken      `json:"tokens,omitempty"`
	ParentID      string            `json:"parentId,omitempty"`
	SubflowID     string            `json:"subflowId"`

	ChildPtrs []*Block `json:"-"`
}

type Subflow struct {
	ID         string         `json:"id"`
	Name       string         `json:"name"`
	SourceFile string         `json:"sourceFile,omitempty"` // file name (without path) this subflow was parsed from
	Blocks     []Block        `json:"blocks"`
	Variables  []VariableDecl `json:"variables"`
}

type VariableDecl struct {
	Name         string `json:"name"`
	Type         string `json:"type"`
	InitialValue string `json:"initialValue,omitempty"`
	Scope        string `json:"scope"`
}
