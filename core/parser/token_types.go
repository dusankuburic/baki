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
	Kind        TokenKind
	Line        int
	Indent      int
	Raw         string
	Content     string
	Name        string
	RawType     string
	SetVarName  string // populated for SET x TO expr tokens
	SetVarValue string // populated for SET x TO expr tokens
}
