# 22. Emails are registered templates, written as real MIME, sent from one runner

Date: 2026-10-10. Status: accepted.

## Context

Six emails were `Send*` methods (plus one rendered in `internal/notify`) built with `fmt.Sprintf`; token links in `href`s were not escaped, the MIME writer used a fixed boundary and copied comment text raw into the text part, so a comment could close the part and add an HTML alternative to an official email. There was no `Date`/`Message-ID`, the Subject was not RFC 2047 encoded, and forgot-password answered slower for existing accounts — an account-existence oracle.

## Decision

1. A template is a typed declaration: `mail.Template[D]{ID, Subject, Text, HTML, Sensitive}` executed against `{.Data, .App Env, .Locale}`; `Env.TokenLink`/`AppLink` build links, so data holds only facts. `mail.Register` parses, executes once against a zero `D` and panics on a parse error, missing field or duplicate ID. IDs are stable (goldens, logs). Templates: `PasswordReset`, `EmailVerification`, `OrgInvite`, `FindingAssigned`, `FindingComment`, `GovernanceAlert`.
2. Escaping is contextual (`html/template`; a non-http(s) URL becomes `#ZgotmplZ`). Subjects are folded to one line and capped at 200 characters.
3. MIME is written by the standard library: `multipart/alternative` with a `crypto/rand` boundary per message, quoted-printable parts, Q-encoded folded Subject, `Date` and `Message-ID`, addresses parsed with `net/mail` (an address with a line break or a list is refused, never stripped); the bare addresses are the SMTP envelope.
4. One runner sends everything: `Service.Enqueue(ctx, to, tmpl.With(data))` renders at once and sends on `lifecycle.NewBounded("mail", 32)`, with 8 slots reserved for `Sensitive` mail (the token emails) so notification bursts cannot crowd out a password reset. Each send is bounded by 45 s. A failed send is reported through `errreport` with the template ID and phase/reply code only (`mail: rcpt: 550`) — a relay's reply text can name the recipient. Governance alerts (`SendAlert`) wait for a slot of the notification share instead (pacing); finding and `analysis_complete` emails are dropped at saturation (counted). The token emails are one job each on the reserved share: token write and send both run on the runner, the request only looks the account up (closing the timing oracle — a failed token write is logged/reported, not a 500 only a known account could get). Callers inside a transaction enqueue from a `txhook.Register` hook.
5. Recipient data policy: `FlowName` is empty unless the recipient can read the flow; the templates then leave the flow clause out.
6. Goldens: `internal/mail/testdata/<id>[-variant].{subject,txt,html,eml}.golden` pin every output; a coverage test fails when a registered template has no fixture.

## Consequences

Every message gains `Date`, `Message-ID` and quoted-printable parts. Mail is still at most once and in process (a crash loses what is queued). A password reset can now be dropped when all 32 slots are busy (logged and counted, `pad_background_dropped_total{task="mail"}`). `PAD_SMTP_MAX_CONNECTIONS` (default 0 = the runner's 32) caps concurrent connections to a relay; non-sensitive mail may hold one less. A new email is one declaration plus a fixture; a call site never formats a body or builds a link.
