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
	sqlVarRef = regexp.MustCompile(`%[A-Za-z_][A-Za-z0-9_]*%`)
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

		if sqlVarRef.MatchString(sqlText) {
			return []models.Finding{{
				RuleID:      r.ID(),
				Severity:    r.DefaultSeverity(),
				Title:       "SQL injection risk",
				Description: "SQL statement interpolates variables directly. A malicious value could alter the query.",
				BlockID:     block.ID,
				SubflowID:   block.SubflowID,
				Suggestion:  "Use parameterized queries (@paramName or ? placeholders) instead of embedding %variables% directly in the SQL string.",
				Metadata:    map[string]interface{}{"property": key},
			}}
		}
	}

	return nil
}

// init self-registers this rule with the analyzer's rule catalog
// (see registry.go) — no separate registration step required.
func init() { registerRule(&SqlInjectionRiskRule{}) }
