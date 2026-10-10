package mail

// OrgInviteData is the org_invite email's data: the organization's name (user-controlled) and the raw invite token.
type OrgInviteData struct {
	OrgName string
	Token   string
}

// OrgInvite invites an address to an organization.
var OrgInvite = Register(Template[OrgInviteData]{
	ID:        "org_invite",
	Sensitive: true,
	Subject:   "You've been invited to PAD Analyzer",
	Text: `You've been invited to join the "{{.Data.OrgName}}" organization on PAD Analyzer.` + "\n\n" +
		"Accept the invitation:\n" +
		`{{.App.TokenLink "invite" .Data.Token}}`,
	HTML: `<p>You've been invited to join the <strong>{{.Data.OrgName}}</strong> organization on PAD Analyzer.</p>` +
		`<p><a href="{{.App.TokenLink "invite" .Data.Token}}">Accept the invitation</a>.</p>`,
})
