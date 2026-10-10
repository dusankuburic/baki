package mail

// PasswordResetData is the password_reset email's data: the raw one-time token (the link is built from Env).
type PasswordResetData struct {
	Token string
}

// PasswordReset answers a forgot-password request.
var PasswordReset = Register(Template[PasswordResetData]{
	ID:        "password_reset",
	Sensitive: true,
	Subject:   "Reset your PAD Analyzer password",
	Text: "We received a request to reset your PAD Analyzer password.\n\n" +
		"Use this link to choose a new password (valid for 1 hour):\n" +
		`{{.App.TokenLink "resetPassword" .Data.Token}}` + "\n\n" +
		"If you didn't request this, you can safely ignore this email.",
	HTML: `<p>We received a request to reset your PAD Analyzer password.</p>` +
		`<p><a href="{{.App.TokenLink "resetPassword" .Data.Token}}">Reset your password</a> (valid for 1 hour).</p>` +
		`<p>If you didn't request this, you can safely ignore this email.</p>`,
})

// EmailVerificationData is the email_verification email's data: the raw one-time token.
type EmailVerificationData struct {
	Token string
}

// EmailVerification confirms a newly registered address (register, resend-verification).
var EmailVerification = Register(Template[EmailVerificationData]{
	ID:        "email_verification",
	Sensitive: true,
	Subject:   "Confirm your PAD Analyzer email",
	Text: "Welcome to PAD Analyzer!\n\n" +
		"Confirm your email address with this link (valid for 24 hours):\n" +
		`{{.App.TokenLink "verifyEmail" .Data.Token}}`,
	HTML: `<p>Welcome to PAD Analyzer!</p>` +
		`<p><a href="{{.App.TokenLink "verifyEmail" .Data.Token}}">Confirm your email address</a> (valid for 24 hours).</p>`,
})
