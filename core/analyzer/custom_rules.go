package analyzer

import (
	"encoding/json"
	"os"
	"regexp"
	"strings"

	"pad-core/models"
)

type CustomRuleConfig struct {
	ID           string            `json:"id"`
	Name         string            `json:"name"`
	Description  string            `json:"description"`
	Severity     string            `json:"severity"`
	Category     string            `json:"category"`
	RawTypeMatch string            `json:"rawTypeMatch"`
	NameMatch    string            `json:"nameMatch"`
	PropertyHas  map[string]string `json:"propertyHas"`
	PropertyNot  map[string]string `json:"propertyNot"`
	TypeMatch    string            `json:"typeMatch"`
	Suggestion   string            `json:"suggestion"`
}

type CustomRule struct {
	config CustomRuleConfig
	sev    models.Severity
	nameRe *regexp.Regexp
	rawRe  *regexp.Regexp
}

func LoadCustomRules(path string) ([]Rule, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- custom-rules path is operator-configured
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var configs []CustomRuleConfig
	if err := json.Unmarshal(data, &configs); err != nil {
		return nil, err
	}

	var rules []Rule
	for _, cfg := range configs {
		r, err := NewCustomRule(cfg)
		if err != nil {
			continue
		}
		rules = append(rules, r)
	}
	return rules, nil
}

func NewCustomRule(cfg CustomRuleConfig) (*CustomRule, error) {
	r := &CustomRule{
		config: cfg,
		sev:    models.SeverityWarning,
	}
	if cfg.Severity != "" {
		r.sev = models.Severity(cfg.Severity)
	}
	if cfg.NameMatch != "" {
		re, err := regexp.Compile("(?i)" + cfg.NameMatch)
		if err != nil {
			return nil, err
		}
		r.nameRe = re
	}
	if cfg.RawTypeMatch != "" {
		re, err := regexp.Compile("(?i)" + cfg.RawTypeMatch)
		if err != nil {
			return nil, err
		}
		r.rawRe = re
	}
	return r, nil
}

func (r *CustomRule) ID() string                       { return r.config.ID }
func (r *CustomRule) Name() string                     { return r.config.Name }
func (r *CustomRule) Description() string              { return r.config.Description }
func (r *CustomRule) DefaultSeverity() models.Severity { return r.sev }
func (r *CustomRule) Category() string                 { return r.config.Category }

func (r *CustomRule) Check(block *models.Block, ctx *RuleContext) []models.Finding {
	if r.config.TypeMatch != "" {
		if !strings.EqualFold(string(block.Type), r.config.TypeMatch) {
			return nil
		}
	}

	if r.rawRe != nil && !r.rawRe.MatchString(block.RawType) {
		return nil
	}

	if r.nameRe != nil && !r.nameRe.MatchString(block.Name) {
		return nil
	}

	if r.config.PropertyHas != nil {
		for key, pattern := range r.config.PropertyHas {
			val, ok := block.Properties[key]
			if !ok {
				return nil
			}
			if !strings.Contains(strings.ToLower(val), strings.ToLower(pattern)) {
				return nil
			}
		}
	}

	if r.config.PropertyNot != nil {
		for key, pattern := range r.config.PropertyNot {
			val, ok := block.Properties[key]
			if ok && strings.Contains(strings.ToLower(val), strings.ToLower(pattern)) {
				return nil
			}
		}
	}

	suggestion := r.config.Suggestion
	if suggestion == "" {
		suggestion = "Review this block for potential issues."
	}

	return []models.Finding{{
		RuleID:      r.ID(),
		Severity:    r.DefaultSeverity(),
		Title:       r.config.Name,
		Description: r.config.Description,
		BlockID:     block.ID,
		SubflowID:   block.SubflowID,
		Suggestion:  suggestion,
		Category:    r.config.Category,
	}}
}
