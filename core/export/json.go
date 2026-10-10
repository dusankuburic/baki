package export

import (
	"encoding/json"

	"pad-core/models"
)

func init() {
	registerBuiltin(Format{ID: "json", Label: "JSON", MIME: "application/json", Ext: ".json", Order: 10}, reportToJSON)
}

// JSONReport is the json format's document: the report with the policy
// verdict (Options.Policy) beside it, as `bakicli -format json` and
// -report-json have always written it.
type JSONReport struct {
	*models.AnalysisReport
	Policy *models.PolicyResult `json:"policy,omitempty"`
}

// reportToJSON indents with two spaces and ends with a newline, byte for byte
// what the CLI's json.Encoder (SetIndent "", "  ") wrote.
func reportToJSON(in Input, opts Options) ([]byte, error) {
	report := in.Report
	if report == nil {
		report = &models.AnalysisReport{}
	}
	b, err := json.MarshalIndent(JSONReport{AnalysisReport: report, Policy: opts.Policy}, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}
