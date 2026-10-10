package export

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"strconv"
	"time"

	"pad-core/analyzer"
	"pad-core/models"
)

// The registered junit format ends the document with a newline, as every
// caller has always written it.
func init() {
	registerBuiltin(Format{ID: "junit", Label: "JUnit XML", MIME: "application/xml; charset=utf-8", Ext: ".xml", Order: 30},
		func(in Input, opts Options) ([]byte, error) {
			b, err := ReportToJUnitWithOptions(in.Report, in.Doc, opts)
			if err != nil {
				return nil, err
			}
			return append(b, '\n'), nil
		})
}

// ReportToJUnit serializes an analysis report as JUnit XML: each finding a
// <testcase>; error/warning findings carry a <failure>, info and preview
// findings are passing testcases noted via <system-out>. doc may be nil.
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
	for i := range report.Findings {
		if junitFails(&report.Findings[i]) {
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
		if junitFails(&f) {
			tc.Failure = &junitFailure{
				Type:    string(f.Severity),
				Message: f.Title,
				Content: body,
			}
		} else {
			// Info and preview findings are passing testcases; the doc comment
			// promised their body in <system-out>, but the struct had no such
			// field, so 12 of 31 testcases on a sample flow rendered with no
			// body at all.
			if !analyzer.Gates(&f) {
				body = junitPreviewNote + body
			}
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
	// []byte(buf.String()) conversion copied the whole rendered document a
	// second time (an extra multi-MB allocation at peak for 10k-finding reports).
	var buf bytes.Buffer
	buf.WriteString(xml.Header)
	enc := xml.NewEncoder(&buf)
	if err := enc.Encode(root); err != nil {
		return nil, fmt.Errorf("JUnit XML encode: %w", err)
	}
	return buf.Bytes(), nil
}

// junitPreviewNote heads a preview finding's <system-out>, so a CI reader
// sees why an error-severity testcase passed.
const junitPreviewNote = "Preview rule: reported, never fails the build.\n\n"

// junitFails reports whether f is a <failure>: a finding that gates (a preview
// finding never does, analyzer.Gates) and is not info; an unknown severity
// fails, as in every gate.
func junitFails(f *models.Finding) bool {
	return f.Severity != models.SeverityInfo && analyzer.Gates(f)
}

// junitCaseName returns "[rule] title @id", unique per finding — "[rule]
// title" alone collapsed N findings of one rule into ONE flapping test in
// Jenkins/GitLab; the fingerprint is the stable identity, BlockID the fallback.
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
	props := []junitProperty{
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
	if res.Preview > 0 {
		props = append(props, junitProperty{Name: "policy.preview", Value: strconv.Itoa(res.Preview)})
	}
	return props
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
