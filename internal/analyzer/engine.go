package analyzer

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"pad-analyzer/internal/logger"
	"pad-analyzer/internal/models"
)

var variableRefRegex = regexp.MustCompile(`%([^%]+)%`)

type RuleContext struct {
	Flow         *models.FlowDocument
	AllBlocks    map[string]*models.Block
	BlocksByName map[string][]*models.Block
	BlocksByType map[models.BlockType][]*models.Block
	Settings     *models.AppSettings
	ParentMap    map[string]string
	SiblingMap   map[string][]*models.Block
	UsedVariables map[string]bool
	BlockIndex   map[string]int
	TerminatorIndex map[string]int
	BlockDepth   map[string]int
	totalBlocks  int
}

type Rule interface {
	ID() string
	Name() string
	Description() string
	DefaultSeverity() models.Severity
	Category() string
	Check(block *models.Block, ctx *RuleContext) []models.Finding
}

func buildContext(flow *models.FlowDocument, settings *models.AppSettings) *RuleContext {
	ctx := &RuleContext{
		Flow:         flow,
		AllBlocks:    make(map[string]*models.Block),
		BlocksByName: make(map[string][]*models.Block),
		BlocksByType: make(map[models.BlockType][]*models.Block),
		ParentMap:    make(map[string]string),
		SiblingMap:   make(map[string][]*models.Block),
		UsedVariables: make(map[string]bool),
		BlockIndex:   make(map[string]int),
		TerminatorIndex: make(map[string]int),
		BlockDepth:   make(map[string]int),
		Settings:     settings,
	}

	for i := range flow.Subflows {
		sf := &flow.Subflows[i]
		collectBlocks(ctx, sf.Blocks, "", sf.ID, 0)
	}

	return ctx
}

func collectBlocks(ctx *RuleContext, blocks []models.Block, parentID string, subflowID string, depth int) {
	ptrs := make([]*models.Block, 0, len(blocks))
	for i := range blocks {
		if blocks[i].Type == models.BlockTypeEnd {
			continue
		}
		ptrs = append(ptrs, &blocks[i])
	}

	terminatorIdx := -1
	for i, b := range ptrs {
		ctx.BlockIndex[b.ID] = i
		if terminatorIdx < 0 && isTerminator(b) {
			terminatorIdx = i
		}

		ctx.AllBlocks[b.ID] = b
		ctx.BlocksByName[b.Name] = append(ctx.BlocksByName[b.Name], b)
		ctx.BlocksByType[b.Type] = append(ctx.BlocksByType[b.Type], b)
		if parentID != "" {
			ctx.ParentMap[b.ID] = parentID
		}
		ctx.BlockDepth[b.ID] = depth
		ctx.totalBlocks++

		// Extract used variables
		if b.Properties != nil {
			for _, val := range b.Properties {
				matches := variableRefRegex.FindAllStringSubmatch(val, -1)
				for _, m := range matches {
					if len(m) > 1 {
						ctx.UsedVariables[strings.TrimSpace(m[1])] = true
					}
				}
			}
		}
		if b.Variables != nil {
			for _, v := range b.Variables {
				matches := variableRefRegex.FindAllStringSubmatch(v, -1)
				for _, m := range matches {
					if len(m) > 1 {
						ctx.UsedVariables[strings.TrimSpace(m[1])] = true
					}
				}
			}
		}
	}
	ctx.SiblingMap[parentID] = ptrs
	if terminatorIdx >= 0 {
		ctx.TerminatorIndex[parentID] = terminatorIdx
	}

	for i := range blocks {
		b := &blocks[i]
		if len(b.Children) > 0 {
			collectBlocks(ctx, b.Children, b.ID, subflowID, depth+1)
		}
	}
}

var terminatorPatterns = []string{
	"ExitSubflow",
	"Exit subflow",
	"End flow",
	"EndFlow",
	"Return",
	"TerminateFlow",
}

func isTerminator(b *models.Block) bool {
	for _, p := range terminatorPatterns {
		if strings.Contains(b.Name, p) || strings.Contains(b.RawType, p) {
			return true
		}
	}
	return false
}

func walkBlocks(flow *models.FlowDocument, fn func(block *models.Block)) {
	for i := range flow.Subflows {
		walkSubflowBlocks(&flow.Subflows[i], fn)
	}
}

func walkSubflowBlocks(sf *models.Subflow, fn func(block *models.Block)) {
	for i := range sf.Blocks {
		walkBlockTree(&sf.Blocks[i], fn)
	}
}

func walkBlockTree(b *models.Block, fn func(block *models.Block)) {
	fn(b)
	for i := range b.Children {
		walkBlockTree(&b.Children[i], fn)
	}
}

func computeStats(findings []models.Finding) models.AnalysisStats {
	stats := models.AnalysisStats{}
	for _, f := range findings {
		switch f.Severity {
		case models.SeverityError:
			stats.Errors++
		case models.SeverityWarning:
			stats.Warnings++
		case models.SeverityInfo:
			stats.Info++
		}
	}
	return stats
}

// safeCheck runs a single rule against a block, recovering from any panic so that
// one buggy rule (or a malformed block) can't abort the entire analysis. The
// offending rule/block is logged and skipped.
func safeCheck(rule Rule, block *models.Block, ctx *RuleContext) (findings []models.Finding) {
	defer func() {
		if r := recover(); r != nil {
			logger.Error("analysis rule panicked; skipping",
				"rule", rule.ID(), "block", block.ID, "panic", r)
			findings = nil
		}
	}()
	return rule.Check(block, ctx)
}

func RunAnalysis(flow *models.FlowDocument, rules []Rule, settings *models.AppSettings,
	onProgress func(current, total int, ruleName string)) *models.AnalysisReport {

	start := time.Now()
	ctx := buildContext(flow, settings)

	var enabledRules []Rule
	for _, r := range rules {
		if settings != nil {
			if rc, ok := settings.Analysis.Rules[r.ID()]; ok {
				if !rc.Enabled {
					continue
				}
			}
		}
		enabledRules = append(enabledRules, r)
	}

	total := len(enabledRules) * ctx.totalBlocks
	var findings []models.Finding
	counter := 0

	for _, rule := range enabledRules {
		walkBlocks(flow, func(block *models.Block) {
			if block.Type == models.BlockTypeEnd {
				return
			}
			counter++
			if onProgress != nil && counter%50 == 0 {
				onProgress(counter, total, rule.Name())
			}
			
			ruleFindings := safeCheck(rule, block, ctx)

			// Apply severity overrides from settings
			if settings != nil {
				if rc, ok := settings.Analysis.Rules[rule.ID()]; ok && rc.Severity != "" {
					for i := range ruleFindings {
						ruleFindings[i].Severity = models.Severity(rc.Severity)
					}
				}
			}
			
			findings = append(findings, ruleFindings...)
		})
		if onProgress != nil {
			onProgress(counter, total, rule.Name())
		}
	}

	if findings == nil {
		findings = []models.Finding{}
	}

	stats := computeStats(findings)
	stats.BlocksAnalyzed = ctx.totalBlocks
	stats.RulesRun = len(enabledRules)

	elapsed := time.Since(start)
	report := &models.AnalysisReport{
		FlowID:      flow.ID,
		GeneratedAt: time.Now(),
		Findings:    findings,
		Stats:       stats,
		DurationMs:  int(elapsed.Milliseconds()),
	}

	for i := range findings {
		findings[i].ID = fmt.Sprintf("F-%03d", i+1)
	}

	return report
}

func GetSiblings(ctx *RuleContext, block *models.Block) []*models.Block {
	parentID := ctx.ParentMap[block.ID]
	return ctx.SiblingMap[parentID]
}

func GetParent(ctx *RuleContext, block *models.Block) *models.Block {
	pid, ok := ctx.ParentMap[block.ID]
	if !ok {
		return nil
	}
	return ctx.AllBlocks[pid]
}

func HasErrorHandlerAncestor(ctx *RuleContext, block *models.Block) bool {
	cur := block
	// visited guards against a cycle in ParentMap (a malformed/rehydrated index
	// could produce one), which would otherwise loop forever.
	visited := make(map[string]bool)
	for {
		if cur == nil || visited[cur.ID] {
			return false
		}
		visited[cur.ID] = true
		pid, ok := ctx.ParentMap[cur.ID]
		if !ok {
			return false
		}
		parent := ctx.AllBlocks[pid]
		if parent == nil {
			// ParentMap references a block absent from AllBlocks (inconsistent
			// index) — stop rather than nil-panic.
			return false
		}
		if parent.Type == models.BlockTypeErrorHandler {
			return true
		}
		for _, s := range ctx.SiblingMap[pid] {
			if s.Type == models.BlockTypeErrorHandler {
				return true
			}
		}
		cur = parent
	}
}

func BuildVariableLineage(doc *models.FlowDocument, varName string) *models.VariableHistory {
	history := &models.VariableHistory{
		Name:   varName,
		Events: make([]models.VariableEvent, 0),
	}

	if doc == nil || varName == "" {
		return history
	}

	for i := range doc.Subflows {
		sf := &doc.Subflows[i]
		walkSubflowBlocks(sf, func(b *models.Block) {
			// Check for assignment/output
			if b.Properties != nil {
				if output, ok := b.Properties["_output"]; ok && output == varName {
					eventType := "init"
					if b.RawType != "SET" && b.RawType != "Variables.SetVariable" {
						eventType = "mutate"
					}
					history.Events = append(history.Events, models.VariableEvent{
						Type:      eventType,
						BlockID:   b.ID,
						Line:      b.LineNumber,
						SubflowID: sf.Name,
					})
					return // If it's an output, we count it once (even if it also reads itself, e.g. x = x + 1)
				}
			}

			// Check for read
			if b.Variables != nil {
				for _, v := range b.Variables {
					if v == varName {
						history.Events = append(history.Events, models.VariableEvent{
							Type:      "read",
							BlockID:   b.ID,
							Line:      b.LineNumber,
							SubflowID: sf.Name,
						})
						break
					}
				}
			}
		})
	}

	return history
}
