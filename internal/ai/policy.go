package ai

import (
	"context"
	"errors"
)

// ProviderPolicy decides whether a chain may be built on providerID for scope (and orgID, when the call is an
// organisation's): the factory consults it before it reads any credential, so a refused provider never sees a
// key and never answers a request (ADR 0028). Refusals wrap ErrProviderNotAllowed; any other error is the
// policy itself failing to decide and is reported as such (fail closed).
type ProviderPolicy interface {
	Allowed(ctx context.Context, scope, orgID, providerID string) error
}

// ErrProviderNotAllowed is the policy's refusal: the deployment disabled the provider, or the organisation's
// allowed-provider list leaves it out. Match with errors.Is; the wire code is PROVIDER_NOT_ALLOWED.
var ErrProviderNotAllowed = errors.New("provider not allowed here")

// ErrProviderPolicyUnavailable: the policy could not be read (the organisation's settings document failed to
// load), so nothing was admitted and nothing refused. Its text is fixed copy a client may show; the cause is
// logged by the policy. The wire code is PROVIDER_POLICY_UNAVAILABLE (503 on the knowledge routes, the code on
// a connection test's result).
var ErrProviderPolicyUnavailable = errors.New("couldn't read the organisation's AI provider policy — try again in a moment")

// Admit asks the factory's policy for a call made around the factory (the Azure AI Foundry probes, which
// build a raw provider from a document under test): the same rule as every builder, before any key is read.
func (f *ProviderFactory) Admit(ctx context.Context, scope, orgID, providerID string) error {
	return f.admitProvider(ctx, scope, orgID, providerID)
}

// admitProvider asks the factory's policy, if any, before a builder resolves a credential. An org flow run on
// a member's own key still names the flow's organisation through WithUsageScope, so the organisation's list
// applies to its content whichever credential the chain runs on.
func (f *ProviderFactory) admitProvider(ctx context.Context, scope, orgID, providerID string) error {
	if f.policy == nil {
		return nil
	}
	if orgID == "" {
		if _, org, ok := UsageScopeFrom(ctx); ok {
			orgID = org
		}
	}
	if err := f.policy.Allowed(ctx, scope, orgID, providerID); err != nil {
		if errors.Is(err, ErrProviderNotAllowed) {
			recordPolicyRefusal(providerID)
		}
		return err
	}
	return nil
}
