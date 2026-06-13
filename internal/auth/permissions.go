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
