package export

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"pad-core/models"
)

// SARIF (Static Analysis Results Interchange Format) 2.1.0 export. The output
// is consumable by GitHub code scanning, Azure DevOps, and other security
// dashboards. Only the subset of the schema we populate is modeled here; the
// signature mirrors ReportToMarkdown/ReportToPDF so callers can pick a format
// uniformly.

const (
	sarifSchema  = "https://json.schemastore.org/sarif-2.1.0.json"
	sarifVersion = "2.1.0"
	sarifTool    = "PAD Analyzer"
)

type sarifLog struct {
	Schema  string     `json:"$schema"`
	Version string     `json:"version"`
	Runs    []sarifRun `json:"runs"`
}

type sarifRun struct {
	Tool    sarifToolBlock `json:"tool"`
	Results []sarifResult  `json:"results"`
}

type sarifToolBlock struct {
	Driver sarifDriver `json:"driver"`
}

type sarifDriver struct {
	Name           string                     `json:"name"`
	InformationURI string                     `json:"informationUri,omitempty"`
	Version        string                     `json:"version,omitempty"`
	Rules          []sarifReportingDescriptor `json:"rules"`
}

// ToolVersion is the analyzer version surfaced in SARIF tool.driver.version
// for result reproducibility/triage. Defaults to a constant; main.go can
// override it at startup (SetToolVersion) to reflect the build version.
var ToolVersion = "0.1.0"

// SetToolVersion sets the version emitted in SARIF tool.driver.version. Called
// once at startup from main so SARIF consumers (GitHub Code Scanning, Azure
// DevOps) can correlate results to a specific analyzer build.
func SetToolVersion(v string) {
	if v != "" {
		ToolVersion = v
	}
}

type sarifReportingDescriptor struct {
	ID                   string              `json:"id"`
	ShortDescription     *sarifMessage       `json:"shortDescription,omitempty"`
	DefaultConfiguration *sarifConfiguration `json:"defaultConfiguration,omitempty"`
	Properties           map[string]any      `json:"properties,omitempty"`
}

type sarifConfiguration struct {
	Level string `json:"level"`
}

type sarifMessage struct {
	Text string `json:"text"`
}

type sarifResult struct {
	RuleID              string            `json:"ruleId"`
	RuleIndex           int               `json:"ruleIndex"`
	Level               string            `json:"level"`
	Message             sarifMessage      `json:"message"`
	Locations           []sarifLocation   `json:"locations,omitempty"`
	PartialFingerprints map[string]string `json:"partialFingerprints,omitempty"`
}

type sarifLocation struct {
	PhysicalLocation *sarifPhysicalLocation `json:"physicalLocation,omitempty"`
	LogicalLocations []sarifLogicalLocation `json:"logicalLocations,omitempty"`
}

type sarifPhysicalLocation struct {
	ArtifactLocation sarifArtifactLocation `json:"artifactLocation"`
	Region           *sarifRegion          `json:"region,omitempty"`
}

type sarifArtifactLocation struct {
	URI string `json:"uri"`
}

type sarifRegion struct {
	StartLine int `json:"startLine"`
}

type sarifLogicalLocation struct {
	Name               string `json:"name"`
	FullyQualifiedName string `json:"fullyQualifiedName,omitempty"`
	Kind               string `json:"kind,omitempty"`
}

// ReportToSARIF serializes an analysis report as SARIF 2.1.0. doc may be nil; when
// supplied it is used to resolve each finding's block to a source file and line
// number for richer physical locations.
func ReportToSARIF(report *models.AnalysisReport, doc *models.FlowDocument) ([]byte, error) {
	if report == nil {
		report = &models.AnalysisReport{}
	}
	// Findings reference blocks by ID; the lookup maps are transient and may be
	// empty on a deserialized document. Rebuild them so locations can resolve.
	if doc != nil && doc.BlocksByID == nil {
		doc.RebuildIndexes()
	}

	ruleIndex := make(map[string]int)
	rules := make([]sarifReportingDescriptor, 0)
	results := make([]sarifResult, 0, len(report.Findings))

	// Track the highest-severity level observed per rule so the rule's
	// declared DefaultConfiguration reflects the worst finding for that rule,
	// not just the first one we happened to encounter (M7).
	ruleMaxRank := make(map[string]int)

	for _, f := range report.Findings {
		idx, ok := ruleIndex[f.RuleID]
		if !ok {
			idx = len(rules)
			ruleIndex[f.RuleID] = idx
			rules = append(rules, ruleDescriptor(f))
			ruleMaxRank[f.RuleID] = severityRank(sarifLevel(f.Severity))
		} else if r := severityRank(sarifLevel(f.Severity)); r > ruleMaxRank[f.RuleID] {
			ruleMaxRank[f.RuleID] = r
			rules[idx].DefaultConfiguration = &sarifConfiguration{Level: sarifLevel(f.Severity)}
		}

		results = append(results, sarifResult{
			RuleID:    f.RuleID,
			RuleIndex: idx,
			Level:     sarifLevel(f.Severity),
			Message:   sarifMessage{Text: messageText(f)},
			Locations: sarifLocations(f, doc),
			PartialFingerprints: map[string]string{
				"padFindingId/v1": fingerprint(f),
			},
		})
	}

	log := sarifLog{
		Schema:  sarifSchema,
		Version: sarifVersion,
		Runs: []sarifRun{{
			Tool:    sarifToolBlock{Driver: sarifDriver{Name: sarifTool, Version: ToolVersion, Rules: rules}},
			Results: results,
		}},
	}

	return json.MarshalIndent(log, "", "  ")
}

func ruleDescriptor(f models.Finding) sarifReportingDescriptor {
	d := sarifReportingDescriptor{
		ID:                   f.RuleID,
		DefaultConfiguration: &sarifConfiguration{Level: sarifLevel(f.Severity)},
	}
	if f.Title != "" {
		d.ShortDescription = &sarifMessage{Text: f.Title}
	}
	if f.Category != "" {
		d.Properties = map[string]any{"category": f.Category}
	}
	return d
}

// sarifLevel maps a finding severity to a SARIF result level. SARIF has no
// "info" level; the closest is "note".
func sarifLevel(s models.Severity) string {
	switch s {
	case models.SeverityError:
		return "error"
	case models.SeverityWarning:
		return "warning"
	case models.SeverityInfo:
		return "note"
	default:
		return "warning"
	}
}

// severityRank orders SARIF levels by severity so we can track the worst level
// observed per rule (M7). Higher rank = more severe.
func severityRank(level string) int {
	switch level {
	case "error":
		return 3
	case "warning":
		return 2
	case "note":
		return 1
	default:
		return 0
	}
}

// messageText builds the human-readable result message. SARIF requires a
// non-empty message.text, so we always fall back to the title and then the rule
// ID.
func messageText(f models.Finding) string {
	msg := f.Description
	if msg == "" {
		msg = f.Title
	}
	if msg == "" {
		msg = f.RuleID
	}
	if f.Suggestion != "" {
		msg += "\n\nSuggestion: " + f.Suggestion
	}
	return msg
}

func sarifLocations(f models.Finding, doc *models.FlowDocument) []sarifLocation {
	if f.BlockID == "" && f.SubflowID == "" {
		return nil
	}

	phys := &sarifPhysicalLocation{
		ArtifactLocation: sarifArtifactLocation{URI: artifactURI(f, doc)},
	}
	if line := blockLine(f, doc); line > 0 {
		phys.Region = &sarifRegion{StartLine: line}
	}

	loc := sarifLocation{PhysicalLocation: phys}
	if f.BlockID != "" {
		fqn := f.BlockID
		if f.SubflowID != "" {
			fqn = f.SubflowID + "/" + f.BlockID
		}
		loc.LogicalLocations = []sarifLogicalLocation{{
			Name:               f.BlockID,
			FullyQualifiedName: fqn,
			Kind:               "member",
		}}
	}
	return []sarifLocation{loc}
}

// artifactURI resolves the source file a finding belongs to, preferring the
// subflow's parsed source file, then a synthesized name from the subflow ID.
func artifactURI(f models.Finding, doc *models.FlowDocument) string {
	if doc != nil && f.SubflowID != "" {
		if sf := doc.SubflowsByID[f.SubflowID]; sf != nil && sf.SourceFile != "" {
			return sf.SourceFile
		}
	}
	if f.SubflowID != "" {
		return f.SubflowID + ".txt"
	}
	return "flow.txt"
}

func blockLine(f models.Finding, doc *models.FlowDocument) int {
	if doc == nil || f.BlockID == "" {
		return 0
	}
	if b := doc.BlocksByID[f.BlockID]; b != nil {
		return b.LineNumber
	}
	return 0
}

// fingerprint is a stable per-finding identity (not array position) so SARIF
// consumers can track a result across re-analysis runs even as other findings
// come and go. It hashes the content-stable Fingerprint (rule + subflow/name/
// line/subject) so a result tracks across desktop re-imports and CLI re-runs —
// hashing the legacy Key (RuleID:BlockID) would mint a fresh id every re-parse.
// Falls back to Key() if Fingerprint is unset (older reports).
func fingerprint(f models.Finding) string {
	k := f.Fingerprint
	if k == "" {
		k = f.Key()
	}
	h := sha256.Sum256([]byte(k))
	return hex.EncodeToString(h[:8])
}
