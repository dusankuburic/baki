package ai

import "context"

type usageScopeKey struct{}

type usageScope struct {
	scope, orgID string
}

// WithUsageScope names who a chain's calls under ctx are for: the knowledge base runs an org corpus's embeds
// under the org whatever key the chain resolved to, and a chat turn its calls under the flow's org, so the usage
// row and the provider policy see the org. It beats the chain on every call kind (Chat, Stream, Embed): the usage
// row records its scope and its org, not the chain's scope or the Request's org (R-budget-15). Absent, the
// wrapper falls back to the chain's own scope and the request's org, else the credential's org.
func WithUsageScope(ctx context.Context, scope, orgID string) context.Context {
	return context.WithValue(ctx, usageScopeKey{}, usageScope{scope: scope, orgID: orgID})
}

// UsageScopeFrom reads WithUsageScope; ok is false when none was set.
func UsageScopeFrom(ctx context.Context) (scope, orgID string, ok bool) {
	s, ok := ctx.Value(usageScopeKey{}).(usageScope)
	return s.scope, s.orgID, ok
}
