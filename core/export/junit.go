package export

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"strconv"
	"time"

	"pad-core/models"
)

// ReportToJUnit serializes an analysis report as JUnit XML — the de-facto CI
// format consumed by Jenkins, GitLab CI, GitHub Actions, etc. Each finding
// becomes a <testcase>; error and warning findings carry a <failure> child
// while info findings appear as passing testcases (noted via <system-out>).
// doc may be nil; when provided, its name populates the testsuite name.
func ReportToJUnit(report *models.AnalysisReport, doc *models.FlowDocument) ([]byte, error) {
	return ReportToJUnitWithOptions(report, doc, Options{})
}

// ReportToJUnitWithOptions is ReportToJUnit plus the policy verdict as a
// <properties> block on the testsuite (policy.id/name/passed/violations…), so a
// CI test report says why the gate failed instead of only listing findings.
func ReportToJUnitWithOptions(report *models.AnalysisReport, doc *models.FlowDocument, opts Options) ([]byte, error) {
	if report == nil {
		report = &models.AnalysisReport{}
	}

	suiteName := "PAD Analysis"
	if doc != nil && doc.Name != "" {
		suiteName = doc.Name
	}

	total := len(report.Findings)
	failures := 0
	for _, f := range report.Findings {
		if f.Severity == models.SeverityError || f.Severity == models.SeverityWarning {
			failures++
		}
	}

	var cases []junitTestcase
	for _, f := range report.Findings {
		tc := junitTestcase{
			Classname: suiteName,
			Name:      junitCaseName(f),
			Time:      "0",
		}
		body := f.Description
		if f.Suggestion != "" {
			body += "\n\nSuggestion: " + f.Suggestion
		}
		if f.Severity == models.SeverityError || f.Severity == models.SeverityWarning {
			tc.Failure = &junitFailure{
				Type:    string(f.Severity),
				Message: f.Title,
				Content: body,
			}
		} else {
			// Info findings are passing testcases. The doc comment above
			// promised their body in <system-out>; the struct had no such
			// field, so 12 of 31 testcases on a sample flow rendered with no
			// body at all — a name, and nothing to act on.
			tc.SystemOut = &junitText{Content: body}
		}
		cases = append(cases, tc)
	}

	suite := junitTestsuite{
		Name:      suiteName,
		Tests:     total,
		Failures:  failures,
		Time:      fmt.Sprintf("%.3f", float64(report.DurationMs)/1000.0),
		Timestamp: report.GeneratedAt.Format(time.RFC3339),
		Testcases: cases,
	}
	if opts.Policy != nil {
		suite.Properties = &junitProperties{Items: policyJUnitProperties(opts.Policy)}
	}

	root := junitRoot{
		Suites: []junitTestsuite{suite},
	}

	// Encode straight into a bytes.Buffer: the previous strings.Builder →
	// []byte(buf.String()) conversion copied the entire rendered document a
	// second time (an extra multi-MB allocation at peak for 10k-finding reports).
	var buf bytes.Buffer
	buf.WriteString(xml.Header)
	enc := xml.NewEncoder(&buf)
	if err := enc.Encode(root); err != nil {
		return nil, fmt.Errorf("JUnit XML encode: %w", err)
	}
	return buf.Bytes(), nil
}

// junitCaseName returns "[rule] title @id", unique per finding. It used to be
// "[rule] title" alone, so N findings of one rule collapsed into ONE test in
// Jenkins/GitLab (they key on classname+name) that flapped between pass and
// fail as findings came and went. The fingerprint is the stable, content-
// derived identity baselines use, so the same finding keeps the same test name
// across runs; the block ID is the fallback for findings built without one.
func junitCaseName(f models.Finding) string {
	id := f.Fingerprint
	if id == "" {
		id = f.BlockID
	}
	if len(id) > 12 {
		id = id[:12]
	}
	if id == "" {
		return fmt.Sprintf("[%s] %s", f.RuleID, f.Title)
	}
	return fmt.Sprintf("[%s] %s @%s", f.RuleID, f.Title, id)
}

// JUnit XML types (using xml names to match the standard format).

type junitRoot struct {
	XMLName xml.Name         `xml:"testsuites"`
	Suites  []junitTestsuite `xml:"testsuite"`
}

type junitTestsuite struct {
	Name       string           `xml:"name,attr"`
	Tests      int              `xml:"tests,attr"`
	Failures   int              `xml:"failures,attr"`
	Errors     int              `xml:"errors,attr"`
	Time       string           `xml:"time,attr"`
	Timestamp  string           `xml:"timestamp,attr"`
	Properties *junitProperties `xml:"properties,omitempty"`
	Testcases  []junitTestcase  `xml:"testcase"`
}

// junitProperties is the standard JUnit <properties><property name= value=/>
// block; JUnit consumers (Jenkins, GitLab, Azure) surface it on the suite.
type junitProperties struct {
	Items []junitProperty `xml:"property"`
}

type junitProperty struct {
	Name  string `xml:"name,attr"`
	Value string `xml:"value,attr"`
}

// policyJUnitProperties flattens a PolicyResult into suite properties.
func policyJUnitProperties(res *models.PolicyResult) []junitProperty {
	return []junitProperty{
		{Name: "policy.id", Value: res.PolicyID},
		{Name: "policy.name", Value: res.PolicyName},
		{Name: "policy.passed", Value: strconv.FormatBool(res.Passed)},
		{Name: "policy.violations", Value: strconv.Itoa(len(res.Violations))},
		{Name: "policy.errors", Value: strconv.Itoa(res.Errors)},
		{Name: "policy.warnings", Value: strconv.Itoa(res.Warnings)},
		{Name: "policy.info", Value: strconv.Itoa(res.Info)},
		{Name: "policy.evaluated", Value: strconv.Itoa(res.Evaluated)},
		{Name: "policy.waived", Value: strconv.Itoa(res.Waived)},
	}
}

type junitTestcase struct {
	Classname string        `xml:"classname,attr"`
	Name      string        `xml:"name,attr"`
	Time      string        `xml:"time,attr"`
	Failure   *junitFailure `xml:"failure,omitempty"`
	SystemOut *junitText    `xml:"system-out,omitempty"`
}

type junitText struct {
	Content string `xml:",chardata"`
}

type junitFailure struct {
	Type    string `xml:"type,attr"`
	Message string `xml:"message,attr"`
	Content string `xml:",chardata"`
}
