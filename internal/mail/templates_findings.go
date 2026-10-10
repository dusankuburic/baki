package mail

import "unicode/utf8"

// FindingAssignedData is the finding_assigned email's data. Names and titles are user/content-controlled.
// FlowName must be "" unless the recipient can read the flow (an editor may assign a non-collaborator); the email
// then does not name the flow.
type FindingAssignedData struct {
	AssigneeName string
	AssignerName string
	FlowName     string
	FindingTitle string
}

// FindingAssigned tells a user a finding was assigned to them. The link goes to the app, where the targeted in-app
// alert is waiting: the frontend has no per-finding route.
var FindingAssigned = Register(Template[FindingAssignedData]{
	ID:      "finding_assigned",
	Subject: "A finding was assigned to you",
	Text: "Hi {{.Data.AssigneeName}},\n\n" +
		`{{.Data.AssignerName}} assigned a finding to you{{with .Data.FlowName}} on the flow "{{.}}"{{end}}:` + "\n\n" +
		"  {{.Data.FindingTitle}}\n" +
		findingLinkText,
	HTML: `<p>Hi {{.Data.AssigneeName}},</p>` +
		`<p><strong>{{.Data.AssignerName}}</strong> assigned a finding to you{{with .Data.FlowName}} on the flow <strong>{{.}}</strong>{{end}}:</p>` +
		`<blockquote>{{.Data.FindingTitle}}</blockquote>` +
		findingLinkHTML,
})

// FindingCommentData is the finding_comment email's data. Comment is the full comment body; the email quotes a
// preview of it (commentPreviewBytes). FlowName follows FindingAssignedData's rule.
type FindingCommentData struct {
	RecipientName string
	CommenterName string
	FlowName      string
	FindingTitle  string
	Comment       string
}

// FindingComment tells a finding's assignee someone commented on it.
var FindingComment = Register(Template[FindingCommentData]{
	ID:      "finding_comment",
	Subject: "New comment on your assigned finding",
	Text: "Hi {{.Data.RecipientName}},\n\n" +
		`{{.Data.CommenterName}} commented on a finding assigned to you{{with .Data.FlowName}} on the flow "{{.}}"{{end}}:` + "\n\n" +
		"  {{preview .Data.Comment}}\n" +
		findingLinkText,
	HTML: `<p>Hi {{.Data.RecipientName}},</p>` +
		`<p><strong>{{.Data.CommenterName}}</strong> commented on a finding assigned to you{{with .Data.FlowName}} on the flow <strong>{{.}}</strong>{{end}}:</p>` +
		`<blockquote>{{preview .Data.Comment}}</blockquote>` +
		findingLinkHTML,
})

// The shared "view it" footer of the finding emails, omitted without a base URL.
const (
	findingLinkText = "{{with .App.AppLink}}\nView it in PAD Analyzer (it is waiting in your notifications):\n{{.}}\n{{end}}"
	findingLinkHTML = `{{with .App.AppLink}}<p><a href="{{.}}">View the finding</a> — it is waiting in your notifications.</p>{{end}}`
)

// commentPreviewBytes bounds the comment excerpt embedded in a notification email.
const commentPreviewBytes = 300

// previewOf truncates a comment body to a preview, cutting on a RUNE boundary: a plain byte slice would split a
// multi-byte character whenever the boundary lands inside one.
func previewOf(s string) string {
	if len(s) <= commentPreviewBytes {
		return s
	}
	cut := commentPreviewBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}
