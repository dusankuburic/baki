package export

import (
	"encoding/xml"
	"fmt"
	"strings"
	"time"

	"pad-core/models"
)

// ReportToJUnit serializes an analysis report as JUnit XML — the de-facto CI
// format consumed by Jenkins, GitLab CI, GitHub Actions, etc. Each finding
// becomes a <testcase>; error and warning findings carry a <failure> child
// while info findings appear as passing testcases (noted via <system-out>).
// doc may be nil; when provided, its name populates the testsuite name.
func ReportToJUnit(report *models.AnalysisReport, doc *models.FlowDocument) ([]byte, error) {
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
			Name:      fmt.Sprintf("[%s] %s", f.RuleID, f.Title),
			Time:      "0",
		}
		if f.Severity == models.SeverityError || f.Severity == models.SeverityWarning {
			body := f.Description
			if f.Suggestion != "" {
				body += "\n\nSuggestion: " + f.Suggestion
			}
			tc.Failure = &junitFailure{
				Type:    string(f.Severity),
				Message: f.Title,
				Content: body,
			}
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

	root := junitRoot{
		Suites: []junitTestsuite{suite},
	}

	var buf strings.Builder
	buf.WriteString(xml.Header)
	enc := xml.NewEncoder(&buf)
	if err := enc.Encode(root); err != nil {
		return nil, fmt.Errorf("JUnit XML encode: %w", err)
	}
	return []byte(buf.String()), nil
}

// JUnit XML types (using xml names to match the standard format).

type junitRoot struct {
	XMLName xml.Name         `xml:"testsuites"`
	Suites  []junitTestsuite `xml:"testsuite"`
}

type junitTestsuite struct {
	Name      string          `xml:"name,attr"`
	Tests     int             `xml:"tests,attr"`
	Failures  int             `xml:"failures,attr"`
	Errors    int             `xml:"errors,attr"`
	Time      string          `xml:"time,attr"`
	Timestamp string          `xml:"timestamp,attr"`
	Testcases []junitTestcase `xml:"testcase"`
}

type junitTestcase struct {
	Classname string        `xml:"classname,attr"`
	Name      string        `xml:"name,attr"`
	Time      string        `xml:"time,attr"`
	Failure   *junitFailure `xml:"failure,omitempty"`
}

type junitFailure struct {
	Type    string `xml:"type,attr"`
	Message string `xml:"message,attr"`
	Content string `xml:",chardata"`
}
