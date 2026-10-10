package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"pad-core/export"
	"pad-core/models"
)

// textFormat is the console report; every other -format is an export format.
const textFormat = "text"

// outputFormats lists the -format values: text, then the registered export
// formats in their Order.
func outputFormats() []string {
	return append([]string{textFormat}, export.IDs()...)
}

func isValidFormat(id string) bool {
	if id == textFormat {
		return true
	}
	_, ok := export.Lookup(id)
	return ok
}

// formatChoices is "text, json, …, or pdf" for error messages.
func formatChoices() string {
	ids := outputFormats()
	return strings.Join(ids[:len(ids)-1], ", ") + ", or " + ids[len(ids)-1]
}

// infoOut is where informational lines go: stdout beside the text report,
// stderr when stdout carries a document (a status line after SARIF or JSON
// makes the file unparseable).
func infoOut(format string) io.Writer {
	if format == textFormat {
		return os.Stdout
	}
	return os.Stderr
}

// checkOutput refuses what cannot be written: a binary format to a terminal,
// and several flows in a format that renders one.
func checkOutput(format string, targets []string, stdoutIsTerminal bool) error {
	if f, ok := export.Lookup(format); ok && f.Binary && stdoutIsTerminal {
		return fmt.Errorf("-format %s is binary and stdout is a terminal; redirect it to a file (> report%s)", format, f.Ext)
	}
	if len(targets) < 2 {
		return nil
	}
	if format != textFormat && format != "json" {
		return fmt.Errorf("-format %s renders one flow; analyze several flows with -format text or json, or run bakicli once per flow", format)
	}
	for _, t := range targets {
		if t == "-" {
			return fmt.Errorf("stdin (-) cannot be analyzed together with other flows")
		}
	}
	return nil
}

// flowRun is one analyzed target of an analyze run.
type flowRun struct {
	target string
	doc    *models.FlowDocument
	report *models.AnalysisReport
	policy *models.PolicyResult
	// revKeys are the finding keys at -new-from-rev.
	revKeys []string
}

// cliFlowReport is one flow of the several-flows JSON document.
type cliFlowReport struct {
	Path string `json:"path"`
	export.JSONReport
}

// cliMultiReport is what -format json and -report-json write for several
// flows: each flow's report (as one flow's JSON), then the cross-flow library
// findings.
type cliMultiReport struct {
	Flows   []cliFlowReport       `json:"flows"`
	Library *models.LibraryReport `json:"library,omitempty"`
}

func multiReport(runs []flowRun, lib *models.LibraryReport) cliMultiReport {
	m := cliMultiReport{Flows: make([]cliFlowReport, len(runs)), Library: lib}
	for i, r := range runs {
		m.Flows[i] = cliFlowReport{Path: r.target, JSONReport: export.JSONReport{AnalysisReport: r.report, Policy: r.policy}}
	}
	return m
}

func marshalJSON(v any) ([]byte, error) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

// writeReportJSON writes -report-json: the json format for one flow, the
// several-flows document otherwise.
func writeReportJSON(path string, runs []flowRun, lib *models.LibraryReport, opts export.Options) error {
	var data []byte
	var err error
	if len(runs) == 1 {
		data, err = export.Bytes("json", export.Input{Report: runs[0].report, Doc: runs[0].doc}, opts)
	} else {
		data, err = marshalJSON(multiReport(runs, lib))
	}
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600) // #nosec G306 -- report file, operator path
}

// renderReport writes the analyze output to stdout.
func renderReport(format string, runs []flowRun, lib *models.LibraryReport, opts export.Options, quiet bool) error {
	if format == textFormat {
		for i, r := range runs {
			if len(runs) > 1 {
				if i > 0 {
					fmt.Println()
				}
				fmt.Printf("== %s ==\n", r.target)
			}
			printText(r.report, quiet)
			if r.policy != nil {
				printPolicyResult(os.Stdout, r.policy, quiet)
			}
		}
		if lib != nil {
			printLibrary(os.Stdout, lib, quiet)
		}
		return nil
	}
	for _, r := range runs {
		warnSkippedRules(r.report)
		if r.policy != nil && !quiet {
			printPolicyResult(os.Stderr, r.policy, true)
		}
	}
	warnSkippedLibraryRules(lib)
	if len(runs) > 1 {
		data, err := marshalJSON(multiReport(runs, lib))
		if err != nil {
			return err
		}
		_, err = os.Stdout.Write(data)
		return err
	}
	f, _ := export.Lookup(format)
	opts.Policy = runs[0].policy
	return f.Render(os.Stdout, export.Input{Report: runs[0].report, Doc: runs[0].doc}, opts)
}

// warnSkippedRules tells stderr that a rule panicked (safeCheck recovered),
// whatever the output format, without touching the stdout report.
func warnSkippedRules(report *models.AnalysisReport) {
	if report.Stats.RulesSkipped > 0 {
		fmt.Fprintf(os.Stderr,
			"warning: %d rule evaluation(s) were skipped due to internal errors; findings may be incomplete.\n",
			report.Stats.RulesSkipped)
	}
}

func warnSkippedLibraryRules(lib *models.LibraryReport) {
	if lib != nil && lib.RulesSkipped > 0 {
		fmt.Fprintf(os.Stderr, "warning: %d library rule(s) were skipped due to internal errors; library findings may be incomplete.\n",
			lib.RulesSkipped)
	}
}

// printLibrary is the text output's "Library (preview)" section: cross-flow
// findings, which never gate.
func printLibrary(w io.Writer, lib *models.LibraryReport, quiet bool) {
	warnSkippedLibraryRules(lib)
	if len(lib.Findings) == 0 && quiet {
		return
	}
	truncated := ""
	if lib.Truncated {
		truncated = ", truncated"
	}
	fmt.Fprintf(w, "\nLibrary (preview): %d finding(s) across %d flows%s; never gated\n\n", len(lib.Findings), lib.Flows, truncated)
	for _, f := range lib.Findings {
		fmt.Fprintf(w, "[%s] %s\n", strings.ToUpper(string(f.Severity)), f.Title)
		fmt.Fprintf(w, "  flow: %s  subflow: %s\n", f.FlowName, f.SubflowName)
		if len(f.Related) > 0 {
			also := make([]string, len(f.Related))
			for i, rel := range f.Related {
				also[i] = fmt.Sprintf("%s (%s)", rel.FlowName, rel.SubflowName)
			}
			fmt.Fprintf(w, "  also in: %s\n", strings.Join(also, ", "))
		}
		if f.Description != "" {
			fmt.Fprintf(w, "  %s\n", f.Description)
		}
		if f.Suggestion != "" {
			fmt.Fprintf(w, "  suggestion: %s\n", f.Suggestion)
		}
		fmt.Fprintln(w)
	}
}
