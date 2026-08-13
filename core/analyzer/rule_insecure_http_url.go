package analyzer

import (
	"strings"

	"pad-core/models"
)

// InsecureHttpUrlRule flags network actions whose target URL uses cleartext
// http:// (no TLS). Credentials, tokens, or PII sent over http:// are
// interceptable on the wire. The existing hardcoded-url rule classifies URLs
// only as Portability; this rule raises the security bar by treating cleartext
// transport as a Security finding (independently of whether the URL is static).
type InsecureHttpUrlRule struct{}

func (r *InsecureHttpUrlRule) ID() string   { return "insecure-http-url" }
func (r *InsecureHttpUrlRule) Name() string { return "Insecure HTTP URL" }
func (r *InsecureHttpUrlRule) Description() string {
	return "Network actions targeting a cleartext http:// URL. Data sent over http:// (credentials, tokens, PII) is interceptable on the wire; use https://."
}
func (r *InsecureHttpUrlRule) DefaultSeverity() models.Severity { return models.SeverityWarning }
func (r *InsecureHttpUrlRule) Category() string                 { return "Security" }

// networkActionPrefixes — PAD actions that hit a URL/endpoint.
var networkActionPrefixes = []string{
	"httpclient.",
	"webautomation.",
	"http.",
	"ftp.",
	"webservice.",
}

// urlPropertyKeys — property keys likely to carry the target URL.
var urlPropertyKeys = map[string]bool{
	"url":       true,
	"endpoint":  true,
	"uri":       true,
	"address":   true,
	"host":      true,
	"baseurl":   true,
	"serverurl": true,
}

func (r *InsecureHttpUrlRule) Check(block *models.Block, ctx *RuleContext) []models.Finding {
	rawLower := strings.ToLower(block.RawType)
	isNet := false
	for _, p := range networkActionPrefixes {
		if strings.HasPrefix(rawLower, p) {
			isNet = true
			break
		}
	}
	if !isNet {
		return nil
	}
	for key, val := range block.Properties {
		if !urlPropertyKeys[strings.ToLower(key)] {
			continue
		}
		v := strings.ToLower(strings.TrimSpace(val))
		// Only a literal http:// (not %var%, not https://) is the cleartext case.
		// A variable URL can't be statically classified; https:// is fine.
		if strings.HasPrefix(v, "http://") {
			return []models.Finding{{
				RuleID:      r.ID(),
				Severity:    r.DefaultSeverity(),
				Title:       "Insecure HTTP URL",
				Description: "Action targets a cleartext http:// URL. Any data sent (credentials, tokens, PII) travels unencrypted and is interceptable on the wire.",
				BlockID:     block.ID,
				SubflowID:   block.SubflowID,
				Suggestion:  "Use the https:// scheme for the target endpoint so transport is TLS-encrypted.",
				Metadata:    map[string]interface{}{"property": key, "url": val},
			}}
		}
	}
	return nil
}

func init() { registerRule(&InsecureHttpUrlRule{}) }
