package notify

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"pad-analyzer/internal/egress"
	"pad-analyzer/internal/mail"
)

// MaxEmailRecipients caps an organisation email channel's recipients: every event is one message per recipient on
// the mail runner's notification share.
const MaxEmailRecipients = 50

// email: a transactional email per event. The deployment's goes to one address (PAD_NOTIFY_EMAIL_TO plus the SMTP
// settings); an organisation's goes to members the admin chose (recipients: member user ids), each at the verified
// address of their account, never to an address the admin types, which would make the server a relay for any text
// a tenant can put in a flow name. The kind is unavailable, and left out without an error, when the deployment has
// no enabled mail sender.
func init() {
	Register(KindSpec{
		ID:    "email",
		Scope: ScopeOrg | ScopeDeployment,
		Fields: []FieldSpec{
			{Name: "to", Required: true, Scope: ScopeDeployment},
			{Name: "recipients", Required: true, Scope: ScopeOrg, Members: true, Check: checkMemberIDs},
		},
		Mail: true,
		Build: func(d Deps, v Values) (AlertNotifier, error) {
			if d.Mailer == nil || !d.Mailer.Enabled() {
				return nil, ErrKindUnavailable
			}
			if v["recipients"] == "" {
				return &EmailNotifier{Sender: d.Mailer, To: v["to"]}, nil
			}
			if len(d.MemberAddresses) == 0 {
				return nil, &FieldError{Field: "recipients", Err: errors.New("none of the recipients is still a member with a verified email address")}
			}
			return &EmailNotifier{Sender: d.Mailer, Recipients: slices.Clone(d.MemberAddresses)}, nil
		},
		Public: func(v Values) Values {
			out := Values{}
			for _, name := range []string{"to", "recipients"} {
				if v[name] != "" {
					out[name] = v[name]
				}
			}
			return out
		},
	})
}

// checkMemberIDs is the recipients field's Check: one to MaxEmailRecipients user ids, comma-separated, each once.
// Whether they are members is the channel service's to check.
func checkMemberIDs(_ context.Context, v string, _ egress.Policy, _ EgressSpec) error {
	ids := strings.Split(v, ",")
	if len(ids) > MaxEmailRecipients {
		return fmt.Errorf("at most %d recipients", MaxEmailRecipients)
	}
	seen := map[string]bool{}
	for _, id := range ids {
		if id == "" || strings.TrimSpace(id) != id || len(id) > 128 || seen[id] {
			return errors.New("recipients are member ids, each once")
		}
		seen[id] = true
	}
	return nil
}

// EmailNotifier renders an event as a transactional email and sends it to a
// configured recipient (typically an ops alias or team mailing list). It sends
// through the EmailSender interface (internal/app wires *mail.Service in) and renders
// with the mail package's governance_alert template (ADR 0022).
type EmailNotifier struct {
	Sender EmailSender
	To     string
	// Recipients are an organisation channel's addresses (its chosen members'), one message each, so no member sees
	// another's address; To is unused then.
	Recipients []string
}

func (n *EmailNotifier) Name() string { return "email" }

func (n *EmailNotifier) Notify(ctx context.Context, ev Event) error {
	msg, err := renderAlertEmail(ev.Stamped(time.Now()))
	if err != nil {
		return err
	}
	if ev.Type == EventAnalysisComplete {
		// One per interactive analysis: dropped when the mail runner is saturated, like the other emails requests
		// raise. A sweep's alerts wait for the relay instead (ADR 0022).
		ctx = mail.WithoutWaiting(ctx)
	}
	to := n.Recipients
	if len(to) == 0 {
		to = []string{n.To}
	}
	// One recipient's failure does not keep the others from theirs. SendAlert's errors name no address.
	var errs []error
	for _, addr := range to {
		if err := n.Sender.SendAlert(ctx, addr, msg.Subject, msg.Text, msg.HTML); err != nil {
			errs = append(errs, err)
			if ctx.Err() != nil {
				break // out of time waiting for the relay: the rest would wait in vain
			}
		}
	}
	return errors.Join(errs...)
}

// renderAlertEmail renders ev with the governance_alert template: the email body, and the lines of Jira's
// description.
func renderAlertEmail(ev Event) (mail.Message, error) {
	return mail.RenderGovernanceAlert(mail.GovernanceAlertData{
		Event: string(ev.Type), FlowID: ev.FlowID, FlowName: ev.FlowName, Title: ev.Title, Message: ev.Message,
		NewErrors: ev.NewErrors, NewWarnings: ev.NewWarnings, HealthScore: ev.HealthScore, PrevHealth: ev.PrevHealth,
		Analysis: (*mail.GovernanceAlertAnalysis)(ev.Analysis), At: ev.At,
	})
}
