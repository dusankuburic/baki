package auth

import (
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const (
	// SSOFlowTTL bounds one SSO round trip, from /start to the IdP's callback.
	SSOFlowTTL = 10 * time.Minute
	// ssoFlowAudience tags the flow cookie's token so no other token passes for it, and it for no other.
	ssoFlowAudience = "pad-sso-flow"
)

// SSOFlow is the per-attempt state an SSO round trip carries in a cookie from /start to the callback: the CSRF
// state, the OIDC nonce, the PKCE verifier, and for an account-link round trip the account to link.
type SSOFlow struct {
	State    string `json:"s"`
	Nonce    string `json:"n"`
	Verifier string `json:"v"`
	// LinkUserID turns the callback into "link this IdP identity to my account"; only /sso/link/start sets it,
	// for the authenticated caller.
	LinkUserID string `json:"l,omitempty"`
}

type ssoFlowClaims struct {
	SSOFlow
	jwt.RegisteredClaims
}

// IssueSSOFlow signs flow for the flow cookie. The cookie used to be plain base64 JSON, and the browser that
// holds it is the one that must not be able to change it: a rewritten LinkUserID linked the caller's IdP
// identity to someone else's account, which that identity could then sign in to.
func (m *Manager) IssueSSOFlow(flow SSOFlow) (string, error) {
	now := time.Now()
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, ssoFlowClaims{
		SSOFlow: flow,
		RegisteredClaims: jwt.RegisteredClaims{
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(SSOFlowTTL)),
			Issuer:    m.issuer,
			Audience:  jwt.ClaimStrings{ssoFlowAudience},
		},
	}).SignedString(m.secret)
	if err != nil {
		return "", fmt.Errorf("auth: sign sso flow: %w", err)
	}
	return token, nil
}

// VerifySSOFlow returns the flow a cookie value carries once its signature, audience, issuer and expiry check
// out.
func (m *Manager) VerifySSOFlow(tokenStr string) (*SSOFlow, error) {
	token, err := jwt.ParseWithClaims(tokenStr, &ssoFlowClaims{}, m.keyFunc,
		jwt.WithAudience(ssoFlowAudience), jwt.WithIssuer(m.issuer), jwt.WithExpirationRequired())
	if err != nil {
		return nil, fmt.Errorf("auth: verify sso flow: %w", err)
	}
	claims, ok := token.Claims.(*ssoFlowClaims)
	if !ok || !token.Valid {
		return nil, errors.New("auth: invalid sso flow claims")
	}
	if claims.State == "" || claims.Nonce == "" || claims.Verifier == "" {
		return nil, errors.New("auth: incomplete sso flow")
	}
	return &claims.SSOFlow, nil
}
