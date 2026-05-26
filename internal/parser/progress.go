package parser

import (
	"pad-analyzer/internal/models"
)

type ProgressCallback func(percent int, message string)

func ParseTextWithProgress(text, fileName string, fileSize int64, onProgress ProgressCallback) (*models.FlowDocument, error) {
	if onProgress == nil || fileSize < 1_000_000 {
		return ParseText(text, fileName, fileSize)
	}

	tokens := Tokenize(text)
	onProgress(10, "Tokenized")

	hasSubflow := false
	for _, tok := range tokens {
		if tok.Kind == TokSubflowStart {
			hasSubflow = true
			break
		}
	}
	if !hasSubflow {
		tokens = wrapImplicitSubflow(tokens, fileName)
	}

	totalTokens := 0
	for _, tok := range tokens {
		if tok.Kind != TokEmpty {
			totalTokens++
		}
	}

	state := newParseState()
	processed := 0
	lastReported := 10

	for _, tok := range tokens {
		if tok.Kind == TokEmpty {
			continue
		}
		processed++
		pct := 10 + (processed * 85 / maxInt(totalTokens, 1))
		if pct >= lastReported+5 {
			onProgress(pct, "Parsing...")
			lastReported = pct
		}
		state.processToken(tok)
	}

	onProgress(95, "Finalizing...")

	subflows, totalBlocks, maxDepth := finalizeSubflows(state.built)
	doc := buildDocument(text, fileName, fileSize, subflows, state.parseErrors, totalBlocks, maxDepth)

	onProgress(100, "Done")
	return doc, nil
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
