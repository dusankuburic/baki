package analyzer

import (
	"regexp"
	"strings"

	"pad-core/models"
)

type SqlInjectionRiskRule struct{}

func (r *SqlInjectionRiskRule) ID() string   { return "sql-injection-risk" }
func (r *SqlInjectionRiskRule) Name() string { return "SQL injection risk" }
func (r *SqlInjectionRiskRule) Description() string {
	return "SQL statements that interpolate variables directly instead of using parameterized queries."
}
func (r *SqlInjectionRiskRule) DefaultSeverity() models.Severity { return models.SeverityWarning }
func (r *SqlInjectionRiskRule) Category() string                 { return "Security" }

var (
	sqlActionTypes = []string{
		"database.execute",
		"sql.execute",
		"database.query",
		"sql.query",
		"database.sql",
	}
	sqlVarRef = regexp.MustCompile(`%([A-Za-z_][A-Za-z0-9_]*)%`)
)

func (r *SqlInjectionRiskRule) Check(block *models.Block, ctx *RuleContext) []models.Finding {
	rawLower := strings.ToLower(block.RawType)
	isSqlAction := false
	for _, t := range sqlActionTypes {
		if strings.Contains(rawLower, t) {
			isSqlAction = true
			break
		}
	}
	if !isSqlAction {
		return nil
	}

	// Property keys preserve source case (PascalCase in PAD, e.g. "Sql"), so
	// compare case-insensitively instead of looking up lowercase keys.
	for key, sqlText := range block.Properties {
		keyLower := strings.ToLower(key)
		if keyLower != "sql" && keyLower != "query" && keyLower != "statement" && keyLower != "command" {
			continue
		}

		matches := sqlVarRef.FindAllStringSubmatch(sqlText, -1)
		if len(matches) == 0 {
			continue
		}
		// Taint grading: escalate to Error when ANY interpolated variable
		// traces back to an untrusted source (an HTTP/Web request body, or a
		// writer that built it by concatenation \u2014 i.e. attacker-influenced
		// data flows into the SQL string). Otherwise stay at the rule's default
		// Warning: the var may be internally sanitized.
		tainted := false
		var taintedVar string
		for _, m := range matches {
			vn := m[1]
			if varTaintedByUntrusted(ctx, vn) {
				tainted = true
				taintedVar = vn
				break
			}
		}
		sev := r.DefaultSeverity()
		desc := "SQL statement interpolates variables directly. A malicious value could alter the query."
		if tainted {
			sev = models.SeverityError
			desc = "SQL statement interpolates a variable (" + taintedVar + ") that traces to an untrusted source (HTTP/Web input or concatenation). This is a confirmed taint path, not just a theoretical risk."
		}
		return []models.Finding{{
			RuleID:      r.ID(),
			Severity:    sev,
			Title:       "SQL injection risk",
			Description: desc,
			BlockID:     block.ID,
			SubflowID:   block.SubflowID,
			Suggestion:  "Use parameterized queries (@paramName or ? placeholders) instead of embedding %variables% directly in the SQL string.",
			Metadata:    map[string]interface{}{"property": key, "tainted": tainted},
		}}
	}

	return nil
}

// init self-registers this rule with the analyzer's rule catalog
// (see registry.go) — no separate registration step required.
// varTaintedByUntrusted reports whether the variable vn is written by a block
// whose value derives from an external/untrusted source (an HTTP/Web request
// body) or by a SET that references a variable already traced to an untrusted
// source (one-hop propagation, conservative intraprocedural). Conservative:
// only confirms a taint trail, never clears one (a var with no known writer is
// treated as untainted, not safe).
func varTaintedByUntrusted(ctx *RuleContext, vn string) bool {
	return varTaintedVisited(ctx, vn, map[string]bool{})
}

// varTaintedVisited is the recursive core of varTaintedByUntrusted. `visited`
// breaks cycles in SET chains (SET A TO %B%, SET B TO %A%) so taint tracing
// terminates. A SET that merely references another variable is only tainted if
// that referenced variable is itself tainted — referencing an internal counter
// (e.g. SET X TO %Counter% + 1) does NOT make X untrusted.
func varTaintedVisited(ctx *RuleContext, vn string, visited map[string]bool) bool {
	if ctx == nil || ctx.WritersByVar == nil || ctx.Flow == nil {
		return false
	}
	if visited[vn] {
		return false // cycle guard
	}
	visited[vn] = true
	for _, wid := range ctx.WritersByVar[vn] {
		w, ok := ctx.AllBlocks[wid]
		if !ok {
			continue
		}
		rt := strings.ToLower(w.RawType)
		if strings.HasPrefix(rt, "httpclient.") ||
			strings.HasPrefix(rt, "webautomation.") ||
			strings.HasPrefix(rt, "ftp.") {
			return true // value sourced from an external request body
		}
		// A SET that references another variable carries that variable's taint
		// forward one hop — trace each referenced variable rather than treating
		// any %ref% as tainted (which would flag internal counters as untrusted).
		if val, ok := w.Properties["_value"]; ok {
			for _, m := range sqlVarRef.FindAllStringSubmatch(val, -1) {
				if varTaintedVisited(ctx, m[1], visited) {
					return true
				}
			}
		}
	}
	return false
}

func init() { registerRule(&SqlInjectionRiskRule{}) }
