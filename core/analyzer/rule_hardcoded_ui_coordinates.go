package analyzer

import (
	"regexp"
	"strings"

	"pad-core/models"
)

// HardcodedUICoordinatesRule flags UI-automation actions that click/interact at
// hardcoded screen coordinates (literal X/Y), the single most fragile pattern in
// RPA flows: a resolution change, UI theme update, or layout tweak makes the
// click miss its target silently. Preferring element selectors (id/name/text)
// over coordinates is the table-stakes reliability fix.
type HardcodedUICoordinatesRule struct{}

func (r *HardcodedUICoordinatesRule) ID() string   { return "hardcoded-ui-coordinates" }
func (r *HardcodedUICoordinatesRule) Name() string { return "Hardcoded UI coordinates" }
func (r *HardcodedUICoordinatesRule) Description() string {
	return "UI actions that click at fixed screen coordinates instead of targeting an element by selector. Coordinate-based clicks break silently on any layout/resolution change."
}
func (r *HardcodedUICoordinatesRule) DefaultSeverity() models.Severity { return models.SeverityWarning }
func (r *HardcodedUICoordinatesRule) Category() string                 { return "Security" }

// uiActionPrefixes — PAD's UI-automation action families that take coordinates.
var uiActionPrefixes = []string{
	"uiautomation.",
	"webautomation.",
	"uiautomation2.",
}

// coordinateProp matches a coordinate-ish property key (case-insensitive),
// e.g. X, Y, CoordinateX, Left, Top, ClickX, OffsetX. A literal numeric value
// (no %var%) for such a key is the smell.
var coordinateProp = regexp.MustCompile(`(?i)^(x|y|left|top|clickx|clicky|offsetx|offsety|coordinatex|coordinatey|screenx|screeny)$`)

func (r *HardcodedUICoordinatesRule) Check(block *models.Block, ctx *RuleContext) []models.Finding {
	rawLower := strings.ToLower(block.RawType)
	isUI := false
	for _, p := range uiActionPrefixes {
		if strings.HasPrefix(rawLower, p) {
			isUI = true
			break
		}
	}
	if !isUI {
		return nil
	}

	for key, val := range block.Properties {
		if !coordinateProp.MatchString(key) {
			continue
		}
		// A literal coordinate (digits, optional sign/decimal) — NOT a variable
		// reference (%var%) which at least parameterizes the value.
		v := strings.TrimSpace(val)
		if v == "" || strings.Contains(v, "%") {
			continue
		}
		if isNumericLiteral(v) {
			return []models.Finding{{
				RuleID:      r.ID(),
				Severity:    r.DefaultSeverity(),
				Title:       "Hardcoded UI coordinate",
				Description: "UI action targets a fixed screen coordinate (" + key + ": " + v + "). Any layout, resolution, or theme change makes the click miss its target silently.",
				BlockID:     block.ID,
				SubflowID:   block.SubflowID,
				Suggestion:  "Target the element by a stable selector (UI element id/name/text) instead of absolute coordinates, or read the coordinate from a variable tied to the live element bounds.",
				Metadata:    map[string]interface{}{"property": key},
			}}
		}
	}
	return nil
}

// isNumericLiteral reports whether s is a plain numeric literal (int or float,
// optional leading sign) — the shape of a hardcoded coordinate.
func isNumericLiteral(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}
	if s[0] == '+' || s[0] == '-' {
		s = s[1:]
	}
	if s == "" {
		return false
	}
	dots := 0
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
		case r == '.':
			dots++
			if dots > 1 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func init() { registerRule(&HardcodedUICoordinatesRule{}) }
