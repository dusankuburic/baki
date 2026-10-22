package ai

import (
	"fmt"
	"pad-core/models"
	"sort"
	"strings"
	"unicode/utf8"
)

const SystemPromptDefault = `You are an expert in Power Automate Desktop (PAD) flow analysis. You help users:
- Understand what their flows do
- Find bugs and reliability issues
- Suggest performance and security improvements
- Refactor complex flows

When analyzing a flow:
- Be concrete and specific. Reference exact block names, properties, and line numbers.
- Suggest precise actions, not vague advice.
- If you spot multiple issues, list the most important first.
- Format code, variable names, and PAD action names with backticks.
- Keep responses focused. The user is debugging, not learning theory.
- When you reference a specific block whose "Block ID" is given in the context, link it as a markdown link with a "block:" URL — e.g. [Run SQL statement](block:abc123) — so the user can click through to it. Only do this for IDs actually present in the context.
- Likewise, when you reference one of the listed Known Issues, link it as a markdown link with a "finding:" URL using its given finding id — e.g. [hardcoded password](finding:hardcoded-credential:abc123) — so the user can jump to it in the findings list. Only use finding ids present in the context.

IMPORTANT: You are read-only by default — you cannot execute flows or change files on your own. The ONLY way to modify the flow is the apply_fix tool, which always shows the user an approval prompt with the exact patch first; NOTHING is written unless they explicitly approve. Prefer propose_fix to show what would change; call apply_fix only when the user asks you to fix the issue. If apply_fix is declined, times out, or unavailable, explain the change so the user can apply it manually — never claim you changed anything you didn't.`

type ContextRequest struct {
	Flow               *models.FlowDocument
	SelectedBlock      *models.Block
	SelectedSubflow    *models.Subflow
	Findings           []models.Finding
	RawSourceFiles     map[string]string
	VariableEvents     map[string][]models.VariableEvent
	TokenBudget        int
	Provider           Provider
	SystemPromptSuffix string
	// SelectedBlockMissing: the conversation is about a block that is no longer in the flow (SelectedBlock is
	// nil); the context says so instead of silently leaving the section out.
	SelectedBlockMissing bool
}

func BuildContext(req ContextRequest) (systemPrompt string, contextMessage string) {
	systemPrompt = SystemPromptDefault
	if req.SystemPromptSuffix != "" {
		systemPrompt = SystemPromptDefault + "\n\n" + req.SystemPromptSuffix
	}

	if req.Flow == nil {
		return systemPrompt, ""
	}

	var b strings.Builder

	b.WriteString("## Flow: ")
	b.WriteString(req.Flow.Name)
	b.WriteString("\n\n")

	hasSources := len(req.RawSourceFiles) > 0

	// Findings come FIRST: the final truncation cuts the tail, and the multi-KB file dumps used to crowd
	// out the Analysis Summary — the analysis rides early, the file dumps are what truncation trims.
	if len(req.Findings) > 0 {
		b.WriteString("\n## Analysis Summary\n\n")
		writeFindingsSummary(&b, req.Findings)
	}

	if hasSources {
		filenames := make([]string, 0, len(req.RawSourceFiles))
		for name := range req.RawSourceFiles {
			filenames = append(filenames, name)
		}
		sort.Strings(filenames)

		fmt.Fprintf(&b, "### Source Files (%d)\n\n", len(filenames))
		for _, name := range filenames {
			// Bounded again here (the chat service bounds each file before scrubbing, same cap): the final
			// truncation keeps only the first TokenBudget tokens, so bytes past the cap can never reach the
			// model — appending them only cost a multi-megabyte builder.
			content := BoundSourceText(req.RawSourceFiles[name], req.TokenBudget)
			fmt.Fprintf(&b, "#### %s\n\n```%s\n```\n\n", name, content)
		}
	}

	if req.SelectedBlock == nil && req.SelectedBlockMissing {
		b.WriteString("\n## Selected Block\n\nThe block this conversation is about is no longer in the flow: a fix or an edit changed or removed it. " +
			"Answer about the flow as a whole, and tell the user if their question needs that block.\n")
	}
	if req.SelectedBlock != nil {
		b.WriteString("\n## Selected Block\n\n")
		writeBlockDetail(&b, req.SelectedBlock)

		if parents := findParentChain(req.Flow, req.SelectedBlock); len(parents) > 0 {
			b.WriteString("\n## Surrounding Context (parent chain)\n\n")
			for i, p := range parents {
				fmt.Fprintf(&b, "%s%s: %s\n", strings.Repeat("  ", i), p.Type, p.Name)
			}
		}

		blockFindings := filterFindingsForBlock(req.Findings, req.SelectedBlock.ID)
		if len(blockFindings) > 0 {
			b.WriteString("\n## Known Issues with This Block\n\n")
			for _, f := range blockFindings {
				fmt.Fprintf(&b, "- [%s] **%s** (finding id: %s): %s\n", f.Severity, f.Title, findingKey(&f), f.Description)
				if f.Suggestion != "" {
					fmt.Fprintf(&b, "  **Suggested fix:** %s\n", f.Suggestion)
				}
				for k, v := range f.Metadata {
					fmt.Fprintf(&b, "  - %s: %v\n", k, v)
				}
			}
		}

		// The ±3 blocks around the selection, from the list it sits in, with the selection marked.
		if siblings, at := siblingsOf(req.Flow, req.SelectedBlock); len(siblings) > 1 {
			b.WriteString("\n## Surrounding Blocks (siblings)\n\n")
			start := max(at-3, 0)
			end := min(at+4, len(siblings))
			for i := start; i < end; i++ {
				marker := ""
				if i == at {
					marker = " ← selected"
				}
				fmt.Fprintf(&b, "  %d. %s: %s%s\n", siblings[i].LineNumber, siblings[i].Type, siblings[i].Name, marker)
			}
		}

		if len(req.VariableEvents) > 0 {
			b.WriteString("\n## Variable History\n\n")
			for varName, events := range req.VariableEvents {
				fmt.Fprintf(&b, "**%%%s%%** (%d events):\n", varName, len(events))
				for _, e := range events {
					fmt.Fprintf(&b, "  - [L%d] %s\n", e.Line, e.Type)
				}
			}
		}
	}

	if !hasSources {
		if req.SelectedSubflow != nil {
			b.WriteString("\n## Subflow Outline\n\n")
			writeSubflowOutline(&b, req.SelectedSubflow, req.TokenBudget/4)
		}

		if req.Provider.EstimateTokens(b.String()) < req.TokenBudget/2 {
			b.WriteString("\n## Flow Overview\n\n")
			writeFlowOverview(&b, req.Flow)
		}
	}

	contextMessage = TruncateToTokenLimit(b.String(), req.TokenBudget)
	return
}

// SourceTruncatedMarker ends a source file BoundSourceText cut short, so the
// model (and a reader of the context preview) can tell the file goes on.
const SourceTruncatedMarker = "…(truncated)"

// sourceBytesPerToken sizes BoundSourceText's cap: tokenBudget × 16 bytes, four times the ~4 bytes a cl100k
// token covers on PAD source, so the budget is exhausted inside the kept part on any real file while the
// scrub and tokeniser see at most that many bytes per file (64 KB at the default 4000-token budget).
const sourceBytesPerToken = 16

// BoundSourceText returns content when it is within tokenBudget × 16 bytes, else its head up to that cap —
// cut after the last line break inside the final 4 KB when there is one, else at a rune boundary — plus
// SourceTruncatedMarker. Applied per selected source file BEFORE the AST scrub (ScrubSourceText ran
// ~0.1–0.8 s on a multi-megabyte file) and again by BuildContext. A non-positive budget leaves content as is.
func BoundSourceText(content string, tokenBudget int) string {
	limit := tokenBudget * sourceBytesPerToken
	if tokenBudget <= 0 || len(content) <= limit {
		return content
	}
	cut := limit
	// A whole last line: a literal split mid-way would leave the AST scrub
	// an unterminated value it cannot mask, and reads badly.
	const lineWindow = 4096
	if nl := strings.LastIndexByte(content[:cut], '\n'); nl >= 0 && cut-nl <= lineWindow {
		cut = nl + 1
	} else {
		for cut > 0 && !utf8.RuneStart(content[cut]) {
			cut--
		}
	}
	return content[:cut] + SourceTruncatedMarker
}

// findingKey is the stable identity the chat UI locates a finding by (analysisStore.findingKey): the
// Fingerprint when present, else "ruleID:blockID" — kept in sync so "finding:" deep-links resolve.
func findingKey(f *models.Finding) string {
	if f.Fingerprint != "" {
		return f.Fingerprint
	}
	return f.RuleID + ":" + f.BlockID
}

func writeBlockDetail(b *strings.Builder, block *models.Block) {
	fmt.Fprintf(b, "**Type:** %s (%s)\n", block.Type, block.RawType)
	fmt.Fprintf(b, "**Name:** %s\n", block.Name)
	if block.ID != "" {
		fmt.Fprintf(b, "**Block ID:** %s\n", block.ID)
	}
	if block.LineNumber > 0 {
		fmt.Fprintf(b, "**Line:** %d\n", block.LineNumber)
	}

	if len(block.Properties) > 0 {
		b.WriteString("\n**Properties:**\n")
		for k, v := range block.Properties {
			fmt.Fprintf(b, "- %s: %s\n", k, v)
		}
	}

	if len(block.Variables) > 0 {
		fmt.Fprintf(b, "\n**Variables referenced:** %s\n", strings.Join(block.Variables, ", "))
	}

	if len(block.Children) > 0 {
		limit := 8
		if limit > len(block.Children) {
			limit = len(block.Children)
		}
		fmt.Fprintf(b, "\n**Nested blocks (%d):**\n", len(block.Children))
		for _, child := range block.Children[:limit] {
			if child.ID != "" {
				fmt.Fprintf(b, "  - [L%d] %s: %s (id: %s)\n", child.LineNumber, child.Type, child.Name, child.ID)
			} else {
				fmt.Fprintf(b, "  - [L%d] %s: %s\n", child.LineNumber, child.Type, child.Name)
			}
		}
		if len(block.Children) > 8 {
			fmt.Fprintf(b, "  ... and %d more\n", len(block.Children)-8)
		}
	}
}

func writeSubflowOutline(b *strings.Builder, sf *models.Subflow, tokenBudget int) {
	fmt.Fprintf(b, "Subflow: %s (%d blocks)\n", sf.Name, len(sf.Blocks))
	used := 0
	for _, block := range sf.Blocks {
		line := fmt.Sprintf("  - %s: %s\n", block.Type, block.Name)
		used += EstimateTokens(line)
		if used > tokenBudget {
			b.WriteString("  ... (truncated)\n")
			break
		}
		b.WriteString(line)
	}
}

func writeFlowOverview(b *strings.Builder, doc *models.FlowDocument) {
	fmt.Fprintf(b, "Flow: %s\n", doc.Name)
	fmt.Fprintf(b, "Subflows: %d, Blocks: %d, Max depth: %d\n",
		doc.Metadata.SubflowCount, doc.Metadata.BlockCount, doc.Metadata.MaxDepth)
	for _, sf := range doc.Subflows {
		fmt.Fprintf(b, "  - %s (%d blocks)\n", sf.Name, len(sf.Blocks))
	}
}

func findParentChain(doc *models.FlowDocument, target *models.Block) []*models.Block {
	var chain []*models.Block
	if doc == nil || target == nil || doc.BlocksByID == nil {
		return chain
	}

	// Build bottom-up (target → root) then reverse in place — O(depth), vs prepend-on-each-iteration O(depth²).
	currentID := target.ParentID
	for currentID != "" {
		parent, ok := doc.BlocksByID[currentID]
		if !ok {
			break
		}
		chain = append(chain, parent)
		currentID = parent.ParentID
	}
	for i, j := 0, len(chain)-1; i < j; i, j = i+1, j-1 {
		chain[i], chain[j] = chain[j], chain[i]
	}
	return chain
}

// siblingsOf returns the ordered list target sits in — its parent's children, or its subflow's top-level
// blocks — and target's index in it; nil and -1 when target cannot be placed. The list includes target, so
// the window around it can be centred on that index (a list without target left the window at the start).
func siblingsOf(doc *models.FlowDocument, target *models.Block) ([]models.Block, int) {
	if doc == nil || target == nil || doc.BlocksByID == nil {
		return nil, -1
	}

	var list []models.Block
	if target.ParentID != "" {
		if parent, ok := doc.BlocksByID[target.ParentID]; ok {
			list = parent.Children
		}
	} else if target.SubflowID != "" {
		if sf, ok := doc.SubflowsByID[target.SubflowID]; ok {
			list = sf.Blocks
		}
	}

	for i := range list {
		if list[i].ID == target.ID {
			return list, i
		}
	}
	return nil, -1
}

func filterFindingsForBlock(findings []models.Finding, blockID string) []models.Finding {
	var result []models.Finding
	for _, f := range findings {
		if f.BlockID == blockID {
			result = append(result, f)
		}
	}
	return result
}

func writeFindingsSummary(b *strings.Builder, findings []models.Finding) {
	counts := make(map[string]int)
	for _, f := range findings {
		counts[f.Title]++
	}

	errors, warnings, info := 0, 0, 0
	for _, f := range findings {
		switch f.Severity {
		case models.SeverityError:
			errors++
		case models.SeverityWarning:
			warnings++
		case models.SeverityInfo:
			info++
		}
	}

	fmt.Fprintf(b, "%d findings: %d errors, %d warnings, %d info\n\n", len(findings), errors, warnings, info)

	// Track first suggestion per title for inclusion in summary
	suggestions := make(map[string]string)
	for _, f := range findings {
		if f.Suggestion != "" {
			if _, seen := suggestions[f.Title]; !seen {
				suggestions[f.Title] = f.Suggestion
			}
		}
	}

	for title, count := range counts {
		if count == 1 {
			fmt.Fprintf(b, "- %s (1 occurrence)\n", title)
		} else {
			fmt.Fprintf(b, "- %s (%d occurrences)\n", title, count)
		}
		if s, ok := suggestions[title]; ok {
			fmt.Fprintf(b, "  → %s\n", s)
		}
	}
}
