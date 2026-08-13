package auth

import (
	"context"
)

type contextKey string

const claimsKey contextKey = "auth_claims"

func WithClaims(ctx context.Context, claims *Claims) context.Context {
	return context.WithValue(ctx, claimsKey, claims)
}

func ClaimsFromContext(ctx context.Context) *Claims {
	c, _ := ctx.Value(claimsKey).(*Claims)
	return c
}

const bootstrapKey contextKey = "auth_allow_bootstrap"

// WithAllowBootstrap marks a context as permitted to claim the bootstrap admin
// role on user creation (the "first user becomes admin" rule). Only the
// registration path sets this; SSO JIT provisioning must NOT, so that on a
// fresh deployment whoever reaches the SSO start URL first cannot claim admin.
// Bootstrap is opt-in: an unset value means no bootstrap (safer default).
func WithAllowBootstrap(ctx context.Context, allow bool) context.Context {
	return context.WithValue(ctx, bootstrapKey, allow)
}

// AllowBootstrap reports whether the context permits the bootstrap-admin rule.
func AllowBootstrap(ctx context.Context) bool {
	b, _ := ctx.Value(bootstrapKey).(bool)
	return b
}
