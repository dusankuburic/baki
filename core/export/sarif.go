package export

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"strings"

	"pad-core/analyzer"
	"pad-core/models"
)

// SARIF (Static Analysis Results Interchange Format) 2.1.0 export, consumable
// by GitHub code scanning, Azure DevOps and other security dashboards. Only
// the populated schema subset is modeled here.

const (
	sarifSchema  = "https://json.schemastore.org/sarif-2.1.0.json"
	sarifVersion = "2.1.0"
	sarifTool    = "PAD Analyzer"
	// sarifAutomationID is run.automationDetails.id: the stable category a
	// consumer files every PAD upload under (GitHub code scanning's `category`).
	sarifAutomationID = "pad-analysis"
	// sarifSourceRootID is the conventional uriBaseId for "the repository
	// checkout" — GitHub resolves relative artifact URIs against it.
	sarifSourceRootID = "%SRCROOT%"
)

// RuleHelpURL is every rule descriptor's helpUri (no per-rule pages;
// `bakicli rules <id>` prints the full description).
var RuleHelpURL = "https://github.com/dusankuburic/baki#features"

type sarifLog struct {
	Schema  string     `json:"$schema"`
	Version string     `json:"version"`
	Runs    []sarifRun `json:"runs"`
}

type sarifRun struct {
	Tool               sarifToolBlock                   `json:"tool"`
	AutomationDetails  *sarifAutomationDetails          `json:"automationDetails,omitempty"`
	Invocations        []sarifInvocation                `json:"invocations,omitempty"`
	OriginalURIBaseIDs map[string]sarifArtifactLocation `json:"originalUriBaseIds,omitempty"`
	Results            []sarifResult                    `json:"results"`
	Properties         map[string]any                   `json:"properties,omitempty"`
}

type sarifAutomationDetails struct {
	ID string `json:"id"`
}

type sarifInvocation struct {
	ExecutionSuccessful bool           `json:"executionSuccessful"`
	Properties          map[string]any `json:"properties,omitempty"`
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

// ToolVersion is surfaced in SARIF tool.driver.version for reproducibility;
// main.go can override it at startup (SetToolVersion) with the build version.
var ToolVersion = "0.1.0"

// SetToolVersion sets the SARIF tool.driver.version so consumers can
// correlate results to a specific analyzer build.
func SetToolVersion(v string) {
	if v != "" {
		ToolVersion = v
	}
}

type sarifReportingDescriptor struct {
	ID                   string                   `json:"id"`
	Name                 string                   `json:"name,omitempty"`
	ShortDescription     *sarifMessage            `json:"shortDescription,omitempty"`
	FullDescription      *sarifMessage            `json:"fullDescription,omitempty"`
	Help                 *sarifMultiformatMessage `json:"help,omitempty"`
	HelpURI              string                   `json:"helpUri,omitempty"`
	DefaultConfiguration *sarifConfiguration      `json:"defaultConfiguration,omitempty"`
	Properties           map[string]any           `json:"properties,omitempty"`
}

type sarifConfiguration struct {
	Level string `json:"level"`
}

type sarifMessage struct {
	Text string `json:"text"`
}

type sarifMultiformatMessage struct {
	Text     string `json:"text"`
	Markdown string `json:"markdown,omitempty"`
}

type sarifResult struct {
	RuleID              string            `json:"ruleId"`
	RuleIndex           int               `json:"ruleIndex"`
	Level               string            `json:"level"`
	Message             sarifMessage      `json:"message"`
	Locations           []sarifLocation   `json:"locations,omitempty"`
	PartialFingerprints map[string]string `json:"partialFingerprints,omitempty"`
	Properties          map[string]any    `json:"properties,omitempty"`
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
	URI       string `json:"uri"`
	URIBaseID string `json:"uriBaseId,omitempty"`
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
	return ReportToSARIFWithOptions(report, doc, Options{})
}

// ReportToSARIFWithOptions is ReportToSARIF plus the CI context in Options: a
// policy verdict (run.properties.policy, invocations[0].executionSuccessful,
// per-result policyViolation) and a source root so artifact URIs are
// repository-relative (originalUriBaseIds["%SRCROOT%"] + uriBaseId).
func ReportToSARIFWithOptions(report *models.AnalysisReport, doc *models.FlowDocument, opts Options) ([]byte, error) {
	if report == nil {
		report = &models.AnalysisReport{}
	}
	// Findings reference blocks by ID; the lookup maps are transient and may be
	// empty on a deserialized document. Rebuild them on a shallow copy so a
	// SHARED doc (e.g. a cached cloud-resolved document handed to concurrent
	// requests) is never mutated from a serializer.
	doc = withIndexes(doc)

	catalog := ruleCatalog()
	violations := policyViolationKeys(opts.Policy)

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
			rules = append(rules, ruleDescriptor(f, catalog[f.RuleID]))
			ruleMaxRank[f.RuleID] = severityRank(sarifLevel(f.Severity))
		} else if r := severityRank(sarifLevel(f.Severity)); r > ruleMaxRank[f.RuleID] {
			ruleMaxRank[f.RuleID] = r
			rules[idx].DefaultConfiguration = &sarifConfiguration{Level: sarifLevel(f.Severity)}
		}

		res := sarifResult{
			RuleID:    f.RuleID,
			RuleIndex: idx,
			Level:     sarifLevel(f.Severity),
			Message:   sarifMessage{Text: messageText(f)},
			Locations: sarifLocations(f, doc, opts),
			PartialFingerprints: map[string]string{
				"padFindingId/v1": fingerprint(f),
			},
		}
		if violations[findingKey(f)] {
			res.Properties = map[string]any{"policyViolation": true}
		}
		results = append(results, res)
	}

	run := sarifRun{
		Tool:              sarifToolBlock{Driver: sarifDriver{Name: sarifTool, Version: ToolVersion, Rules: rules}},
		AutomationDetails: &sarifAutomationDetails{ID: sarifAutomationID},
		Results:           results,
	}
	if opts.SourceRootURI != "" {
		uri := opts.SourceRootURI
		if !strings.HasSuffix(uri, "/") {
			uri += "/"
		}
		run.OriginalURIBaseIDs = map[string]sarifArtifactLocation{sarifSourceRootID: {URI: uri}}
	}
	// executionSuccessful is the field consumers read for "did this run pass";
	// a failed policy gate is reported there so a dashboard never shows a green
	// run whose gate failed. Verdict details ride in run.properties.policy.
	inv := sarifInvocation{ExecutionSuccessful: true}
	if opts.Policy != nil {
		inv.ExecutionSuccessful = opts.Policy.Passed
		run.Properties = map[string]any{"policy": policyProperties(opts.Policy)}
	}
	run.Invocations = []sarifInvocation{inv}

	log := sarifLog{
		Schema:  sarifSchema,
		Version: sarifVersion,
		Runs:    []sarifRun{run},
	}

	return json.MarshalIndent(log, "", "  ")
}

// withIndexes returns doc with its transient lookup maps populated, rebuilding
// them on a shallow copy so a shared document is never mutated by an exporter.
func withIndexes(doc *models.FlowDocument) *models.FlowDocument {
	if doc != nil && doc.BlocksByID == nil {
		cp := *doc
		cp.RebuildIndexes()
		return &cp
	}
	return doc
}

// ruleCatalog indexes built-in rules by ID so descriptors carry the rule's
// own stable name/description rather than the first finding's instance
// title. Custom rules fall back to the finding's text.
func ruleCatalog() map[string]analyzer.Rule {
	all := analyzer.AllRules()
	m := make(map[string]analyzer.Rule, len(all))
	for _, r := range all {
		m[r.ID()] = r
	}
	return m
}

// policyProperties is the compact verdict embedded as run.properties.policy:
// PolicyResult counts plus the violating findings' fingerprints (same
// padFindingId/v1 values as the results) instead of the full findings.
func policyProperties(res *models.PolicyResult) map[string]any {
	fps := make([]string, 0, len(res.Violations))
	for _, v := range res.Violations {
		fps = append(fps, fingerprint(v))
	}
	return map[string]any{
		"policyId":              res.PolicyID,
		"policyName":            res.PolicyName,
		"passed":                res.Passed,
		"errors":                res.Errors,
		"warnings":              res.Warnings,
		"info":                  res.Info,
		"evaluated":             res.Evaluated,
		"waived":                res.Waived,
		"violations":            len(res.Violations),
		"violationFingerprints": fps,
	}
}

// policyViolationKeys returns the identity set of a policy's violations so each
// result can be tagged; nil when there is no policy.
func policyViolationKeys(res *models.PolicyResult) map[string]bool {
	if res == nil || len(res.Violations) == 0 {
		return nil
	}
	keys := make(map[string]bool, len(res.Violations))
	for _, v := range res.Violations {
		keys[findingKey(v)] = true
	}
	return keys
}

func findingKey(f models.Finding) string {
	if f.Fingerprint != "" {
		return f.Fingerprint
	}
	return f.Key()
}

func ruleDescriptor(f models.Finding, rule analyzer.Rule) sarifReportingDescriptor {
	d := sarifReportingDescriptor{
		ID:                   f.RuleID,
		DefaultConfiguration: &sarifConfiguration{Level: sarifLevel(f.Severity)},
		HelpURI:              RuleHelpURL,
	}
	name, desc := f.Title, ""
	if rule != nil {
		name, desc = rule.Name(), rule.Description()
		d.Name = rule.Name()
	}
	if name != "" {
		d.ShortDescription = &sarifMessage{Text: name}
	}
	if desc != "" {
		d.FullDescription = &sarifMessage{Text: desc}
	}
	if help := desc; help != "" || f.Description != "" {
		if help == "" {
			help = f.Description
		}
		if fix := analyzer.RuleAutoFix(f.RuleID); fix != "" {
			help += "\n\nAuto-fix available: " + fix + " (apply with `bakicli fix`)."
		}
		help += "\n\nRun `bakicli rules " + f.RuleID + "` for the full rule description."
		d.Help = &sarifMultiformatMessage{Text: help, Markdown: help}
	}
	props := map[string]any{}
	if f.Category != "" {
		props["category"] = f.Category
	} else if rule != nil && rule.Category() != "" {
		props["category"] = rule.Category()
	}
	if rule != nil {
		props["confidence"] = string(analyzer.RuleConfidence(f.RuleID))
	}
	if len(props) > 0 {
		d.Properties = props
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

func sarifLocations(f models.Finding, doc *models.FlowDocument, opts Options) []sarifLocation {
	if f.BlockID == "" && f.SubflowID == "" {
		return nil
	}

	uri := artifactURI(f, doc)
	if root := strings.Trim(opts.SourceRoot, "/"); root != "" && root != "." {
		uri = root + "/" + uri
	}
	phys := &sarifPhysicalLocation{
		ArtifactLocation: sarifArtifactLocation{URI: uri},
	}
	if opts.SourceRootURI != "" {
		phys.ArtifactLocation.URIBaseID = sarifSourceRootID
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
	// Deliberately NOT the subflow ID: a parser-minted UUID regenerated every
	// parse produced a filename matching nothing and differing run to run. A
	// fixed, obviously-synthetic name groups consistently and is greppable.
	if doc != nil && doc.FilePath != "" {
		return filepath.Base(doc.FilePath)
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

// fingerprint is a stable per-finding identity (not array position): hashes
// the content-stable Fingerprint so a result tracks across desktop re-imports
// and CLI re-runs — hashing the legacy Key (RuleID:BlockID) would mint a
// fresh id every re-parse. Falls back to Key() for older reports.
func fingerprint(f models.Finding) string {
	k := f.Fingerprint
	if k == "" {
		k = f.Key()
	}
	h := sha256.Sum256([]byte(k))
	return hex.EncodeToString(h[:8])
}
