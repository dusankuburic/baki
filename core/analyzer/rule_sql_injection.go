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
		// Taint grading: escalate to Error when ANY interpolated variable traces
		// back to an untrusted source (an HTTP/Web request body, or a writer that
		// built it by concatenation); otherwise stay at the default Warning —
		// the var may be internally sanitized.
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

// varTaintedByUntrusted reports whether vn derives from an untrusted source
// (HTTP/Web/FTP) or a SET chain referencing one — conservative: it only
// confirms a taint trail, never clears one. O(1) lookup into ctx.TaintedVars
// (computed once per analysis by buildTaintedVars); nil answers false, exactly
// as the old walk did.
func varTaintedByUntrusted(ctx *RuleContext, vn string) bool {
	if ctx == nil || ctx.TaintedVars == nil {
		return false
	}
	return ctx.TaintedVars[vn]
}

// isTaintSourceType reports whether a writer block of this raw type produces
// a value sourced from an external request body (HTTP/Web/FTP).
func isTaintSourceType(rawType string) bool {
	rt := strings.ToLower(rawType)
	return strings.HasPrefix(rt, "httpclient.") ||
		strings.HasPrefix(rt, "webautomation.") ||
		strings.HasPrefix(rt, "ftp.")
}

// buildTaintedVars computes the variables whose value traces to an untrusted
// source, as a fixed point over ctx.WritersByVar: tainted if any writer is a
// taint-source block (httpclient./webautomation./ftp.), or any writer has a
// _value referencing (%ref%) a tainted variable — a SET carries taint forward
// one hop, to any depth. Semantics match the former recursive walk: cycle-safe,
// conservative, and referencing an internal counter (SET X TO %Counter% + 1)
// does NOT taint X. Cost: one sqlVarRef pass per writer block (cached per
// block ID) plus a linear propagation.
func buildTaintedVars(ctx *RuleContext) map[string]bool {
	tainted := make(map[string]bool)
	if ctx == nil || ctx.WritersByVar == nil {
		return tainted
	}
	// dependents[R] lists the variables written by a block whose _value
	// references %R%: when R turns out tainted, each of them is too.
	dependents := make(map[string][]string)
	refsByBlock := make(map[string][]string)
	var queue []string
	mark := func(vn string) {
		if !tainted[vn] {
			tainted[vn] = true
			queue = append(queue, vn)
		}
	}
	for vn, writers := range ctx.WritersByVar {
		for _, wid := range writers {
			w, ok := ctx.AllBlocks[wid]
			if !ok {
				continue
			}
			if isTaintSourceType(w.RawType) {
				mark(vn) // value sourced from an external request body
				continue
			}
			refs, seen := refsByBlock[wid]
			if !seen {
				if val, ok := w.Properties["_value"]; ok {
					for _, m := range sqlVarRef.FindAllStringSubmatch(val, -1) {
						refs = append(refs, m[1])
					}
				}
				refsByBlock[wid] = refs
			}
			for _, r := range refs {
				dependents[r] = append(dependents[r], vn)
			}
		}
	}
	// Propagate: every variable reachable from a source through reference
	// edges is tainted. Each variable is enqueued at most once.
	for len(queue) > 0 {
		vn := queue[0]
		queue = queue[1:]
		for _, d := range dependents[vn] {
			mark(d)
		}
	}
	return tainted
}

func init() { registerRule(&SqlInjectionRiskRule{}) }
