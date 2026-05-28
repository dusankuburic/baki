package auth

import (
	"context"
	"errors"
)

// contextKey is the unexported type used for context keys in this package.
type contextKey string

const claimsKey contextKey = "auth_claims"

// ErrUnauthorized is returned when an operation requires authentication.
var ErrUnauthorized = errors.New("auth: unauthorized")

// ErrForbidden is returned when the caller lacks the required permission.
var ErrForbidden = errors.New("auth: forbidden")

// WithClaims attaches JWT claims to a context.
func WithClaims(ctx context.Context, claims *Claims) context.Context {
	return context.WithValue(ctx, claimsKey, claims)
}

// ClaimsFromContext extracts JWT claims from a context.
// Returns nil if no claims are present.
func ClaimsFromContext(ctx context.Context) *Claims {
	c, _ := ctx.Value(claimsKey).(*Claims)
	return c
}

// Require returns ErrUnauthorized if no claims are present, or ErrForbidden if
// the authenticated user does not hold the requested permission.
func Require(ctx context.Context, p Permission) error {
	claims := ClaimsFromContext(ctx)
	if claims == nil {
		return ErrUnauthorized
	}
	if !claims.Role.Has(p) {
		return ErrForbidden
	}
	return nil
}

// RequireAny returns nil if the user holds at least one of the given permissions.
func RequireAny(ctx context.Context, perms ...Permission) error {
	claims := ClaimsFromContext(ctx)
	if claims == nil {
		return ErrUnauthorized
	}
	for _, p := range perms {
		if claims.Role.Has(p) {
			return nil
		}
	}
	return ErrForbidden
}
