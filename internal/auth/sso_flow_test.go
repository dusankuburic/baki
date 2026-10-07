package auth

import (
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func TestSSOFlow_RoundTripsAndRefusesOtherTokens(t *testing.T) {
	m := NewManager("test-secret", nil)
	flow := SSOFlow{State: "s", Nonce: "n", Verifier: "v", LinkUserID: "u1"}
	signed, err := m.IssueSSOFlow(flow)
	if err != nil {
		t.Fatal(err)
	}
	got, err := m.VerifySSOFlow(signed)
	if err != nil || *got != flow {
		t.Fatalf("VerifySSOFlow = %+v, %v; want %+v", got, err, flow)
	}

	ticket, err := m.IssueSSOTicket("u1", "u1@example.com", RoleMember)
	if err != nil {
		t.Fatal(err)
	}
	pair, err := m.Issue("u1", "u1@example.com", RoleMember)
	if err != nil {
		t.Fatal(err)
	}
	expired, err := jwt.NewWithClaims(jwt.SigningMethodHS256, ssoFlowClaims{
		SSOFlow: flow,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(-time.Minute)),
			Issuer:    m.issuer,
			Audience:  jwt.ClaimStrings{ssoFlowAudience},
		},
	}).SignedString(m.secret)
	if err != nil {
		t.Fatal(err)
	}
	incomplete, err := m.IssueSSOFlow(SSOFlow{State: "s", LinkUserID: "u1"})
	if err != nil {
		t.Fatal(err)
	}
	for name, tok := range map[string]string{
		"sso ticket":    ticket,
		"access token":  pair.AccessToken,
		"refresh token": pair.RefreshToken,
		"expired flow":  expired,
		"incomplete":    incomplete,
		"not a token":   "eyJzIjoicyJ9",
	} {
		if _, err := m.VerifySSOFlow(tok); err == nil {
			t.Errorf("%s accepted as an SSO flow", name)
		}
	}
}
