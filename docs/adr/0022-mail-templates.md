# 22. Emails are registered templates, written as real MIME, sent from one runner

Date: 2026-10-10. Status: accepted.

## Context

The product sends six emails: password reset, email verification, org invite, finding assigned, finding comment and
the governance alert of the deployment email channel. Five were `Send*` methods on `mail.Service` and the sixth was
rendered in `internal/notify`, all with `fmt.Sprintf`. Escaping was done by hand and unevenly: user text went
through `html.EscapeString` (or notify's own `htmlEscape`), the token links in the reset, verification and invite
`href`s were not escaped at all, and the text parts quoted names with Go's `%q` (`"Acme \"EU\""`). The MIME writer
used a fixed boundary, `pad_mime_boundary_8a3f`, and copied the comment text raw into the text part, so a comment
could close the part and add an HTML alternative of its own to an official email (B6). There was no `Date` or
`Message-ID` header, the Subject was not RFC 2047 encoded, and From was written as configured, so a display-name
`PAD_EMAIL_FROM` broke `MAIL FROM`. Mail went out three ways: inline in the request (reset, verification), on an
invite runner and on a finding runner. The inline sends made forgot-password and resend-verification slower only for
existing accounts, which told an attacker which accounts exist (B7). The assignment email named the flow even to an
assignee who cannot read it. No rendered email was pinned by a test.

## Decision

1. **A template is a typed declaration.** `mail.Template[D]{ID, Subject, Text, HTML, Sensitive}` holds a
   `text/template` subject and text body and an `html/template` HTML body, executed against
   `{.Data D, .App Env, .Locale}`. `Env` carries the public base URL with `TokenLink` and `AppLink`, so templates
   build their own links and the data holds only facts (a token, a name). `mail.Register` parses the template,
   executes it once against a zero `D` and panics on a parse error, a missing field or a duplicate ID, so a broken
   template fails the boot and every test of the package. Each email is one package-level variable in a
   `templates_*.go` file: `PasswordReset`, `EmailVerification`, `OrgInvite`, `FindingAssigned`, `FindingComment`,
   `GovernanceAlert`. IDs are stable (goldens, logs and the `template` log attribute use them).
2. **Escaping is contextual.** `html/template` escapes user text in elements and links in attributes, and replaces a
   non-http(s) URL with `#ZgotmplZ`. Subjects are folded to one line (every run of white space, CR and LF
   included, becomes a space) and capped at 200 characters.
3. **The governance alert is a template too.** `GovernanceAlertData` mirrors `notify.Event` field for field
   (`GovernanceAlertAnalysis` converts directly from `*notify.AnalysisSummary`). `mail.RenderGovernanceAlert(d)
   (mail.Message, error)` needs no `Service`; the notify email kind calls it and hands the result to its
   `EmailSender`, `Service.SendAlert`, which keeps its signature.
4. **MIME is written by the standard library.** `multipart/alternative` through `mime/multipart` with a boundary
   drawn from `crypto/rand` per message, after the bodies are rendered, so no user text can contain it; every part
   is quoted-printable (CRLF lines of at most 76 bytes, 7-bit safe); the Subject is Q-encoded with `mime.QEncoding`
   and folded; `Date` and `Message-ID` (`<random@sender-domain>`) are set; From and To are parsed with `net/mail`
   and formatted, and the bare addresses are the SMTP envelope. An address with a line break or a list is refused,
   never stripped.
5. **One runner sends everything.** `Service.Enqueue(ctx, to, tmpl.With(data))` renders at once (so a bad address or
   template is an error to the caller) and hands the SMTP session to `lifecycle.NewBounded("mail", 32)`. Of the 32
   slots, 8 are reserved for `Sensitive` mail (the token emails: reset, verification, invite), so a burst of finding
   notifications against a slow relay cannot crowd out a password reset. A refused send returns `ErrNotQueued` and
   is logged and counted like any dropped background task. A failed send is logged in full (WARN, local log) and
   reported by the runner through `errreport` (location `mail.send`, the template ID, never the body) as its phase
   and reply code only, `mail: rcpt: 550` (or `timeout`): a relay's reply text can name the recipient. That holds
   for every email alike: only the governance alert's failures reached the error sink before, through notify. Each
   send is bounded by 45 s, and the SMTP connection is closed when that context ends.
   Governance alerts (`SendAlert`) have backpressure instead: an alert waits for a slot of the notification share
   while its context lives (notify's 10 s delivery timeout), so the scanner's one-after-another `Dispatch` is paced
   by the relay, as when it sent inline, and `ErrNotQueued` means the context ended first. The finding and
   `analysis_complete` emails are still dropped at saturation: they come from requests.
   The token emails (reset, verification) are one job each (`Service.EnqueueJob`) on the reserved share: writing the
   token (after invalidating the outstanding ones, for a resend) and sending its link both run on the runner, and
   the request only looks the account up, which an unknown address pays too.
   Shutdown drains the runner with every other one (`lifecycle.DrainAll`). Callers inside a transaction enqueue from
   a `txhook.Register` hook, as the invite already did. The invite and finding runners are gone, and the
   reset and verification emails no longer wait in the request.
6. **Recipient data policy.** `FindingAssignedData.FlowName` and `FindingCommentData.FlowName` are empty unless the
   recipient can read the flow; the templates then leave the flow clause out. Both finding emails apply it now
   (the assignment email did not).
7. **English only.** `Locale` exists so a second language is a template change, not an API change: every locale
   renders the English bodies today and `Message.Locale` says so. No translation machinery.
8. **Goldens.** `internal/mail/testdata/<id>[-variant].{subject,txt,html,eml}.golden` pin every template's output
   and the whole MIME message (date, boundary and Message-ID fixed) under `-update-mail-golden`. They were first
   generated from the pre-registry code, so the diff of this change shows exactly what moved. A coverage test fails
   when a registered template has no fixture or golden.

## Consequences

- Text and HTML bodies are byte-identical to the old ones except where intended: the text parts quote names
  without Go escaping, `+` is escaped as `&#43;` in HTML, and a finding email without a readable flow drops
  `on the flow ""`. Every message gains Date, Message-ID and quoted-printable parts.
- Forgot-password and resend-verification answer in the same time whether or not the account exists (B7 is
  closed): both pay the account lookup, and nothing else is done on the request for a known account but handing a
  job to the runner (and queueing its audit row). A failed token write is logged and reported (`mail.prepare`), no
  longer a 500 that only a known account could get. Without a mail service no token is written.
- A password reset can now be dropped when all 32 slots are busy: the user sees the usual answer and no email.
  Such a drop is logged and counted (`pad_background_dropped_total{task="mail"}`), where it used to hold the
  request instead. The metric labels `finding_email` and `org_invite_email` are replaced by `mail`.
- Mail is still at most once and in process: a crash loses what is queued, as before (ADR 0009 defers an outbox).
- A cap per relay, `PAD_SMTP_MAX_CONNECTIONS` (default 0: only the runner's 32): a send waits, within its 45 s, for
  one of that many connections before it dials, and non-sensitive mail may hold one less, so a password reset never
  waits behind notifications. Without it, a relay that limits concurrent connections below 32 refuses the excess,
  which fails those sends (logged and reported).
- A new email is one declaration plus a fixture; a call site never formats a body or builds a link.
