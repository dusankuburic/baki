package ai

import "context"

type usageScopeKey struct{}

type usageScope struct {
	scope, orgID string
}

// WithUsageScope names who a chain's calls under ctx are for, for calls that carry no Request of their own
// (Embed): the knowledge base runs an org corpus's embeds under the org whatever key the chain resolved to, so
// the usage row and the provider policy see the org. Absent, the wrapper falls back to the chain's own scope
// and the request's org, else the credential's org.
func WithUsageScope(ctx context.Context, scope, orgID string) context.Context {
	return context.WithValue(ctx, usageScopeKey{}, usageScope{scope: scope, orgID: orgID})
}

// UsageScopeFrom reads WithUsageScope; ok is false when none was set.
func UsageScopeFrom(ctx context.Context) (scope, orgID string, ok bool) {
	s, ok := ctx.Value(usageScopeKey{}).(usageScope)
	return s.scope, s.orgID, ok
}
