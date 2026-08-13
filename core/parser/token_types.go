package parser

type TokenKind int

const (
	TokSubflowStart TokenKind = iota
	TokSubflowEnd
	TokAction
	TokLoopStart
	TokEnd
	TokConditionStart
	TokConditionElse
	TokComment
	TokErrorHandlerStart
	TokBlockStart
	TokSwitchStart
	TokCase
	TokDefault
	TokEmpty
	TokError
)

type Token struct {
	Kind TokenKind
	Line int
	// EndLine is the last physical line the token spans. For a single-line
	// token it is 0 (meaning "same as Line"); for a multi-line triple-quoted
	// ACTION value it is the line of the closing '''. Used so fixers that
	// append/wrap/remove a block land after the literal, not inside it.
	EndLine     int
	Indent      int
	Raw         string
	Content     string
	Name        string
	RawType     string
	SetVarName  string // populated for SET x TO expr tokens
	SetVarValue string // populated for SET x TO expr tokens
}
