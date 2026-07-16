package analyzer

import (
	"regexp"
	"strconv"
	"strings"

	"pad-core/models"
)

type HardcodedIPRule struct{}

func (r *HardcodedIPRule) ID() string   { return "hardcoded-ip" }
func (r *HardcodedIPRule) Name() string { return "Hardcoded IP address" }
func (r *HardcodedIPRule) Description() string {
	return "Hardcoded IP addresses in network-related properties that should be parameterized for different environments."
}
func (r *HardcodedIPRule) DefaultSeverity() models.Severity { return models.SeverityInfo }
func (r *HardcodedIPRule) Category() string                 { return "Portability" }

var ipv4Pattern = regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}\b`)

func (r *HardcodedIPRule) Check(block *models.Block, ctx *RuleContext) []models.Finding {
	var findings []models.Finding
	seen := make(map[string]bool)

	for key, val := range block.Properties {
		if strings.Contains(val, "%") {
			continue
		}
		lowerKey := strings.ToLower(key)
		if !strings.Contains(lowerKey, "url") &&
			!strings.Contains(lowerKey, "host") &&
			!strings.Contains(lowerKey, "server") &&
			!strings.Contains(lowerKey, "address") &&
			!strings.Contains(lowerKey, "ip") &&
			!strings.Contains(lowerKey, "endpoint") &&
			!strings.Contains(lowerKey, "connection") {
			continue
		}

		for _, m := range ipv4Pattern.FindAllString(val, -1) {
			if !isValidIPv4(m) {
				continue
			}
			if m == "127.0.0.1" || m == "0.0.0.0" || m == "255.255.255.255" {
				continue
			}
			if seen[m] {
				continue
			}
			seen[m] = true
			findings = append(findings, models.Finding{
				RuleID:      r.ID(),
				Severity:    r.DefaultSeverity(),
				Title:       "Hardcoded IP address",
				Description: "Property '" + key + "' contains a hardcoded IP address: " + m,
				BlockID:     block.ID,
				SubflowID:   block.SubflowID,
				Suggestion:  "Store the IP address in a variable so it can be changed per environment.",
				Metadata:    map[string]interface{}{"property": key, "ip": m},
			})
		}
	}

	return findings
}

func isValidIPv4(s string) bool {
	parts := strings.Split(s, ".")
	if len(parts) != 4 {
		return false
	}
	for _, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || n > 255 {
			return false
		}
	}
	return true
}

func init() { registerRule(&HardcodedIPRule{}) }
