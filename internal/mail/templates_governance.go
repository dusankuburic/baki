package mail

import "time"

// GovernanceAlertData is the governance_alert email's data: the fields of a notify.Event, mirrored so internal/mail
// does not import notify. Event is the event type ("drift", "health_regression", "analysis_complete", "test",
// "new_rules"); the counts are read per type as the channel payloads read them.
type GovernanceAlertData struct {
	Event       string
	FlowID      string
	FlowName    string
	Title       string
	Message     string
	NewErrors   int
	NewWarnings int
	HealthScore int
	PrevHealth  int
	Analysis    *GovernanceAlertAnalysis
	At          time.Time
}

// GovernanceAlertAnalysis mirrors notify.AnalysisSummary field for field, so a *notify.AnalysisSummary converts to
// it directly.
type GovernanceAlertAnalysis struct {
	Errors      int
	Warnings    int
	Info        int
	HealthScore int
}

// GovernanceAlert is the body of the deployment email channel (PAD_NOTIFY_EMAIL_TO).
var GovernanceAlert = Register(Template[GovernanceAlertData]{
	ID:      "governance_alert",
	Subject: "[{{upper .Data.Event}}] {{.Data.Title}}",
	Text: "Flow: {{or .Data.FlowName .Data.FlowID}}\n" +
		"Event: {{.Data.Event}}\n" +
		"{{with .Data.Message}}\n{{.}}\n{{end}}" +
		`{{if eq .Data.Event "drift"}}` +
		"\nNew errors: {{.Data.NewErrors}}\nNew warnings: {{.Data.NewWarnings}}\n" +
		`{{else if eq .Data.Event "health_regression"}}` +
		"\nHealth: {{.Data.PrevHealth}} → {{.Data.HealthScore}}\n" +
		`{{else if eq .Data.Event "analysis_complete"}}{{with .Data.Analysis}}` +
		"\nErrors: {{.Errors}}\nWarnings: {{.Warnings}}\nInfo: {{.Info}}\n" +
		"{{if gt .HealthScore 0}}Health: {{.HealthScore}}/100\n{{end}}" +
		"{{end}}{{end}}" +
		"\nTime: {{rfc3339 .Data.At}}\n",
	HTML: `<table cellpadding="0" cellspacing="0" style="font-family:sans-serif;font-size:14px">` +
		alertRow("Flow", "{{or .Data.FlowName .Data.FlowID}}") +
		alertRow("Event", "{{.Data.Event}}") +
		`{{if eq .Data.Event "drift"}}` +
		alertRow("New errors", "{{.Data.NewErrors}}") +
		alertRow("New warnings", "{{.Data.NewWarnings}}") +
		`{{else if eq .Data.Event "health_regression"}}` +
		alertRow("Health", "{{.Data.PrevHealth}} → {{.Data.HealthScore}}") +
		`{{else if eq .Data.Event "analysis_complete"}}{{with .Data.Analysis}}` +
		alertRow("Errors", "{{.Errors}}") +
		alertRow("Warnings", "{{.Warnings}}") +
		alertRow("Info", "{{.Info}}") +
		`{{if gt .HealthScore 0}}` + alertRow("Health", "{{.HealthScore}}/100") + `{{end}}` +
		`{{end}}{{end}}` +
		`</table>` +
		`{{with .Data.Message}}<p>{{.}}</p>{{end}}`,
})

// alertRow is one label/value row of the alert table; label is a literal, value a template action.
func alertRow(label, value string) string {
	return `<tr><td style="color:#666;padding:2px 12px 2px 0">` + label + `</td><td>` + value + `</td></tr>`
}

// RenderGovernanceAlert renders the deployment email channel's body for one event. It needs no Service (the alert
// carries no link); the notify email kind passes the result to its EmailSender (Service.SendAlert).
func RenderGovernanceAlert(d GovernanceAlertData) (Message, error) {
	return GovernanceAlert.Render(d, Env{}, DefaultLocale)
}
