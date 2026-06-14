package analyzer

import (
	"regexp"
	"strings"

	"pad-analyzer/internal/models"
)

type SqlInjectionRiskRule struct{}

func (r *SqlInjectionRiskRule) ID() string          { return "sql-injection-risk" }
func (r *SqlInjectionRiskRule) Name() string         { return "SQL injection risk" }
func (r *SqlInjectionRiskRule) Description() string  { return "SQL statements that interpolate variables directly instead of using parameterized queries." }
func (r *SqlInjectionRiskRule) DefaultSeverity() models.Severity { return models.SeverityWarning }
func (r *SqlInjectionRiskRule) Category() string     { return "Security" }

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

	for _, key := range []string{"sql", "query", "statement", "command"} {
		sqlText, ok := block.Properties[key]
		if !ok {
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
