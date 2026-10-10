package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"pad-core/analyzer"
	"pad-core/models"
)

// baselineSchemaVersion is the newest baseline file this bakicli reads. It
// writes version 1 (a bare JSON array of finding keys), which every bakicli
// reads; version 2 is {"schemaVersion":2,"keys":[…],"ruleSet":{…}}.
const baselineSchemaVersion = 2

// baselineFile is a baseline as read from disk.
type baselineFile struct {
	Keys []string
	// Found is false when no file exists at the path yet ("no baseline").
	Found bool
	// RuleSet is the rules the baseline was taken with (version 2 files); nil
	// for version 1.
	RuleSet *models.RuleSet
}

// loadBaseline reads a baseline file; a missing file is not an error.
func loadBaseline(path string) (baselineFile, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- baseline path is a CLI argument supplied by the operator
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return baselineFile{}, nil
		}
		return baselineFile{}, fmt.Errorf("read baseline %s: %w", path, err)
	}
	if t := bytes.TrimSpace(data); len(t) > 0 && t[0] == '{' {
		return parseBaselineObject(path, t)
	}
	var keys []string
	if err := json.Unmarshal(data, &keys); err != nil {
		return baselineFile{}, fmt.Errorf("invalid baseline JSON %s: %w", path, err)
	}
	return baselineFile{Keys: keys, Found: true}, nil
}

// parseBaselineObject reads a versioned baseline. The version is checked
// first, so a newer file is reported as newer even if its other fields would
// not decode here; unknown fields are ignored.
func parseBaselineObject(path string, data []byte) (baselineFile, error) {
	var head struct {
		SchemaVersion int `json:"schemaVersion"`
	}
	if err := json.Unmarshal(data, &head); err != nil {
		return baselineFile{}, fmt.Errorf("invalid baseline JSON %s: %w", path, err)
	}
	switch {
	case head.SchemaVersion > baselineSchemaVersion:
		return baselineFile{}, fmt.Errorf("baseline %s has schemaVersion %d: it was written by a newer bakicli (this one reads up to %d)",
			path, head.SchemaVersion, baselineSchemaVersion)
	case head.SchemaVersion != 2:
		return baselineFile{}, fmt.Errorf("invalid baseline %s: an object baseline needs \"schemaVersion\": 2", path)
	}
	var v2 struct {
		Keys    []string        `json:"keys"`
		RuleSet *models.RuleSet `json:"ruleSet"`
	}
	if err := json.Unmarshal(data, &v2); err != nil {
		return baselineFile{}, fmt.Errorf("invalid baseline JSON %s: %w", path, err)
	}
	return baselineFile{Keys: v2.Keys, Found: true, RuleSet: v2.RuleSet}, nil
}

// findingKeys is a report's baseline keys: fingerprints, the legacy key as
// fallback, without duplicates.
func findingKeys(report *models.AnalysisReport) []string {
	var keys []string
	seen := make(map[string]bool)
	for _, f := range report.Findings {
		k := f.Fingerprint
		if k == "" {
			k = f.Key()
		}
		if !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}
	return keys
}

// baselineScope is a flow's key prefix in a several-flows baseline. A
// fingerprint names no flow (every one-file flow's subflow is "Main"), so an
// unscoped union would let one flow's baselined finding hide the same new
// finding in another. The prefix is the path relative to the working
// directory when inside it, so the file survives a moved checkout.
func baselineScope(target string) string {
	p := target
	if abs, err := filepath.Abs(target); err == nil {
		if cwd, err := os.Getwd(); err == nil {
			if rel, err := filepath.Rel(cwd, abs); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				p = rel
			}
		}
	}
	return filepath.ToSlash(filepath.Clean(p)) + "#"
}

// baselineKeys is what -update-baseline writes for runs: one flow's keys as
// they are, several flows' keys each under its flow's baselineScope (still a
// version 1 array; a bakicli that predates scoping matches none of them and
// gates on everything).
func baselineKeys(runs []flowRun) []string {
	if len(runs) == 1 {
		return findingKeys(runs[0].report)
	}
	var keys []string
	seen := make(map[string]bool)
	for _, r := range runs {
		scope := baselineScope(r.target)
		for _, k := range findingKeys(r.report) {
			if k = scope + k; !seen[k] {
				seen[k] = true
				keys = append(keys, k)
			}
		}
	}
	return keys
}

// keysInScope is the part of a several-flows baseline that belongs to the
// flow at target, with the scope removed.
func keysInScope(keys []string, target string) []string {
	scope := baselineScope(target)
	var own []string
	for _, k := range keys {
		if rest, ok := strings.CutPrefix(k, scope); ok {
			own = append(own, rest)
		}
	}
	return own
}

// writeBaseline writes keys as a version 1 file ("[]" when there are none).
func writeBaseline(path string, keys []string) error {
	if keys == nil {
		keys = []string{}
	}
	data, err := json.MarshalIndent(keys, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644) // #nosec G306 -- baseline is a shared, repo-committed artifact; group-readable is intended
}

// newerRuleFindings counts the new findings whose rule is newer than the
// baseline's rule set (they still gate; the count is informational).
func newerRuleFindings(bl baselineFile, report *models.AnalysisReport, drift *models.BaselineDrift) int {
	split := analyzer.SplitDrift(drift.New, analyzer.NewRuleAge(bl.RuleSet, report.RuleSet))
	return len(split.NewRule)
}
