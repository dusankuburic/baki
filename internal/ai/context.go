package ai

import (
	"fmt"
	"pad-analyzer/internal/models"
	"sort"
	"strings"
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

IMPORTANT: You are READ-ONLY. You cannot execute or modify flows. Your job is to explain, analyze, and recommend. Never suggest you can change files or take actions on behalf of the user.`

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

	if hasSources {
		filenames := make([]string, 0, len(req.RawSourceFiles))
		for name := range req.RawSourceFiles {
			filenames = append(filenames, name)
		}
		sort.Strings(filenames)

		fmt.Fprintf(&b, "### Source Files (%d)\n\n", len(filenames))
		for _, name := range filenames {
			content := req.RawSourceFiles[name]
			fmt.Fprintf(&b, "#### %s\n\n```%s\n```\n\n", name, content)
		}
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
				fmt.Fprintf(&b, "- [%s] **%s**: %s\n", f.Severity, f.Title, f.Description)
				if f.Suggestion != "" {
					fmt.Fprintf(&b, "  **Suggested fix:** %s\n", f.Suggestion)
				}
				for k, v := range f.Metadata {
					fmt.Fprintf(&b, "  - %s: %v\n", k, v)
				}
			}
		}

		if siblings := findSiblings(req.Flow, req.SelectedBlock); len(siblings) > 0 {
			b.WriteString("\n## Surrounding Blocks (siblings)\n\n")
			sibIdx := -1
			for i, s := range siblings {
				if s.ID == req.SelectedBlock.ID {
					sibIdx = i
					break
				}
			}
			start := sibIdx - 3
			if start < 0 {
				start = 0
			}
			end := sibIdx + 4
			if end > len(siblings) {
				end = len(siblings)
			}
			for i := start; i < end; i++ {
				marker := ""
				if siblings[i].ID == req.SelectedBlock.ID {
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

	if len(req.Findings) > 0 && req.Provider.EstimateTokens(b.String()) < req.TokenBudget*3/4 {
		b.WriteString("\n## Analysis Summary\n\n")
		writeFindingsSummary(&b, req.Findings)
	}

	contextMessage = TruncateToTokenLimit(b.String(), req.TokenBudget)
	return
}

func writeBlockDetail(b *strings.Builder, block *models.Block) {
	fmt.Fprintf(b, "**Type:** %s (%s)\n", block.Type, block.RawType)
	fmt.Fprintf(b, "**Name:** %s\n", block.Name)
	if block.LineNumber > 0 {
		fmt.Fprintf(b, "**Line:** %d\n", block.LineNumber)
	}

	if len(block.Properties) > 0 {
		b.WriteString("\n**Properties:**\n")
		for k, v := range block.Properties {
			if isPotentialSecret(v) {
				v = maskSecret(v)
			}
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
			fmt.Fprintf(b, "  - [L%d] %s: %s\n", child.LineNumber, child.Type, child.Name)
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

	currentID := target.ParentID
	for currentID != "" {
		parent, ok := doc.BlocksByID[currentID]
		if !ok {
			break
		}
		// Prepend to chain so it's top-down
		chain = append([]*models.Block{parent}, chain...)
		currentID = parent.ParentID
	}
	return chain
}

func findSiblings(doc *models.FlowDocument, target *models.Block) []models.Block {
	if doc == nil || target == nil || doc.BlocksByID == nil {
		return nil
	}

	var allBlocks []models.Block
	if target.ParentID != "" {
		if parent, ok := doc.BlocksByID[target.ParentID]; ok {
			allBlocks = parent.Children
		}
	} else if target.SubflowID != "" {
		if sf, ok := doc.SubflowsByID[target.SubflowID]; ok {
			allBlocks = sf.Blocks
		}
	}

	var siblings []models.Block
	for _, b := range allBlocks {
		if b.ID != target.ID {
			siblings = append(siblings, b)
		}
	}
	return siblings
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
