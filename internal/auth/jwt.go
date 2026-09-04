package auth

import (
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
)

// Claims are the custom JWT claims stored in every access token.
//
// SrcJTI / SrcExp are ONLY populated on WebSocket connect tickets: they carry
// the JTI and expiry of the ACCESS token that authorized the ticket-issuance
// request, so the WebSocket handler can re-check the access token's revocation
// (logout / explicit revoke) and close the connection when the access token
// expires. They are omitted (omitempty) on ordinary access tokens.
type Claims struct {
	UserID string           `json:"uid"`
	Email  string           `json:"email"`
	Role   Role             `json:"role"`
	SrcJTI string           `json:"src_jti,omitempty"`
	SrcExp *jwt.NumericDate `json:"src_exp,omitempty"`
	// Scopes carries a PAT's capability restriction (empty = unscoped = full
	// access). Only ever set on PAT-derived claims (ClaimsForPAT); JWT logins
	// are interactive and unrestricted.
	Scopes []string `json:"scopes,omitempty"`
	jwt.RegisteredClaims
}

// TokenScoped reports whether these claims came from a scope-restricted PAT.
func (c *Claims) TokenScoped() bool { return len(c.Scopes) > 0 }

// AllowsScope reports whether the claims permit the given scope. Unscoped
// claims allow everything.
func (c *Claims) AllowsScope(scope string) bool {
	if !c.TokenScoped() {
		return true
	}
	for _, s := range c.Scopes {
		if s == scope {
			return true
		}
	}
	return false
}

// RefreshClaims are the claims stored in a refresh token.
// Email and Role are stored so a refreshed access token carries the same identity.
type RefreshClaims struct {
	UserID string `json:"uid"`
	Email  string `json:"email"`
	Role   Role   `json:"role"`
	jwt.RegisteredClaims
}

// TokenPair holds an access token and a refresh token.
type TokenPair struct {
	AccessToken  string    `json:"accessToken"`
	RefreshToken string    `json:"refreshToken"`
	ExpiresAt    time.Time `json:"expiresAt"`
	// RefreshID is the refresh token's unique ID (jti). It lets a server-side
	// store track and revoke individual refresh tokens for rotation.
	RefreshID        string    `json:"refreshId"`
	RefreshExpiresAt time.Time `json:"refreshExpiresAt"`
}

const (
	defaultAccessTTL  = 15 * time.Minute
	defaultRefreshTTL = 24 * time.Hour
	// wsTicketTTL bounds how long a WebSocket connect ticket is valid. Tickets
	// are exchanged for a live connection within seconds of issuance, so the
	// window is deliberately tiny to limit replay if one leaks (e.g. via a
	// proxy access log that records the ?ticket= query parameter).
	wsTicketTTL = 30 * time.Second
	// wsTicketAudience tags tickets so they cannot be used as API access tokens
	// (and access tokens cannot be used as tickets).
	wsTicketAudience = "pad-ws-ticket"
	// ssoTicketTTL bounds how long an SSO login exchange ticket is valid. The
	// ticket rides the OIDC callback redirect (URL fragment) and is exchanged
	// for a token pair immediately by the SPA, so the window stays small.
	ssoTicketTTL = 60 * time.Second
	// ssoTicketAudience tags SSO exchange tickets so they are not usable as
	// access tokens, refresh tokens, or WS tickets (and vice versa).
	ssoTicketAudience = "pad-sso-ticket"
	// refreshAudience tags refresh tokens with a distinct audience from access
	// tokens. Without this, a refresh token (same secret, structurally
	// identical claims) would pass Verify() and be usable as a bearer access
	// token — bypassing the short access TTL and the rotation/replay checks
	// that only run at /auth/refresh. Verify() enforces m.audience and
	// VerifyRefresh() enforces this, so neither token works in the other's role.
	refreshAudience = "pad-refresh"
)

// Manager handles JWT issuance and verification.
type Manager struct {
	secret     []byte
	accessTTL  time.Duration
	refreshTTL time.Duration
	issuer     string
	audience   string
	blacklist  BlacklistStore
}

// NewManager creates a Manager using the provided HMAC-SHA256 secret.
// blacklist may be nil to disable token revocation.
func NewManager(secret string, blacklist BlacklistStore) *Manager {
	return NewManagerWithTTL(secret, defaultAccessTTL, defaultRefreshTTL, "pad-analyzer", "pad-client", blacklist)
}

// NewManagerWithTTL creates a Manager with explicit TTL values (useful for tests).
func NewManagerWithTTL(secret string, accessTTL, refreshTTL time.Duration, issuer, audience string, blacklist BlacklistStore) *Manager {
	return &Manager{
		secret:     []byte(secret),
		accessTTL:  accessTTL,
		refreshTTL: refreshTTL,
		issuer:     issuer,
		audience:   audience,
		blacklist:  blacklist,
	}
}

// Issue creates a new TokenPair for the given user.
func (m *Manager) Issue(userID, email string, role Role) (*TokenPair, error) {
	now := time.Now()
	expiresAt := now.Add(m.accessTTL)

	accessClaims := Claims{
		UserID: userID,
		Email:  email,
		Role:   role,
		RegisteredClaims: jwt.RegisteredClaims{
			ID:        uuid.NewString(),
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(expiresAt),
			Subject:   userID,
			Issuer:    m.issuer,
			Audience:  jwt.ClaimStrings{m.audience},
		},
	}

	accessToken, err := jwt.NewWithClaims(jwt.SigningMethodHS256, accessClaims).SignedString(m.secret)
	if err != nil {
		return nil, fmt.Errorf("auth: sign access token: %w", err)
	}

	refreshExpiresAt := now.Add(m.refreshTTL)
	refreshID := uuid.NewString()
	refreshClaims := RefreshClaims{
		UserID: userID,
		Email:  email,
		Role:   role,
		RegisteredClaims: jwt.RegisteredClaims{
			ID:        refreshID,
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(refreshExpiresAt),
			Subject:   userID,
			Issuer:    m.issuer,
			Audience:  jwt.ClaimStrings{refreshAudience},
		},
	}

	refreshToken, err := jwt.NewWithClaims(jwt.SigningMethodHS256, refreshClaims).SignedString(m.secret)
	if err != nil {
		return nil, fmt.Errorf("auth: sign refresh token: %w", err)
	}

	return &TokenPair{
		AccessToken:      accessToken,
		RefreshToken:     refreshToken,
		ExpiresAt:        expiresAt,
		RefreshID:        refreshID,
		RefreshExpiresAt: refreshExpiresAt,
	}, nil
}

// IssueWSTicket creates a short-lived, single-use ticket the client exchanges
// for a WebSocket connection. It keeps the long-lived access token out of the
// WS URL (which is otherwise recorded in proxy/server logs and browser history).
//
// accessJTI / accessExp are the JTI and expiry of the access token that
// authorized this request; they are embedded in the ticket so the WebSocket
// handler can re-check the access token's revocation (logout) and enforce its
// expiry on the live socket. Returns the signed ticket and its expiry.
func (m *Manager) IssueWSTicket(userID, email string, role Role, accessJTI string, accessExp time.Time) (string, time.Time, error) {
	now := time.Now()
	expiresAt := now.Add(wsTicketTTL)
	claims := Claims{
		UserID: userID,
		Email:  email,
		Role:   role,
		SrcJTI: accessJTI,
	}
	if !accessExp.IsZero() {
		claims.SrcExp = jwt.NewNumericDate(accessExp)
	}
	claims.RegisteredClaims = jwt.RegisteredClaims{
		ID:        uuid.NewString(),
		IssuedAt:  jwt.NewNumericDate(now),
		ExpiresAt: jwt.NewNumericDate(expiresAt),
		Subject:   userID,
		Issuer:    m.issuer,
		Audience:  jwt.ClaimStrings{wsTicketAudience},
	}
	ticket, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(m.secret)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("auth: sign ws ticket: %w", err)
	}
	return ticket, expiresAt, nil
}

// VerifyWSTicket parses and validates a WebSocket connect ticket. It enforces
// the ticket audience so an ordinary access token cannot be presented in its
// place. Single-use enforcement (replay prevention) is the caller's job.
func (m *Manager) VerifyWSTicket(tokenStr string) (*Claims, error) {
	token, err := jwt.ParseWithClaims(tokenStr, &Claims{}, m.keyFunc,
		jwt.WithAudience(wsTicketAudience), jwt.WithIssuer(m.issuer))
	if err != nil {
		return nil, fmt.Errorf("auth: verify ws ticket: %w", err)
	}
	claims, ok := token.Claims.(*Claims)
	if !ok || !token.Valid {
		return nil, errors.New("auth: invalid ws ticket claims")
	}
	return claims, nil
}

// ConsumeWSTicket verifies a WS ticket and immediately marks its jti as
// consumed via the shared blacklist (AddIfAbsent), making the ticket truly
// single-use across all replicas. In local mode (in-memory blacklist) this
// is equivalent to the previous process-local map.
func (m *Manager) ConsumeWSTicket(tokenStr string) (*Claims, error) {
	token, err := jwt.ParseWithClaims(tokenStr, &Claims{}, m.keyFunc,
		jwt.WithAudience(wsTicketAudience), jwt.WithIssuer(m.issuer))
	if err != nil {
		return nil, fmt.Errorf("auth: verify ws ticket: %w", err)
	}
	claims, ok := token.Claims.(*Claims)
	if !ok || !token.Valid {
		return nil, errors.New("auth: invalid ws ticket claims")
	}
	if m.blacklist != nil && claims.ID != "" {
		ttl := time.Until(claims.ExpiresAt.Time)
		if ttl <= 0 {
			return nil, errors.New("auth: ws ticket already used")
		}
		added, err := m.blacklist.AddIfAbsent(claims.ID, ttl)
		if err != nil {
			// Fail-closed: the single-use check could not be performed, so
			// reject the ticket. The entry was NOT inserted, so the caller
			// may retry once the store recovers — this avoids the replay
			// window where a bare "already used" on a DB error would both
			// reject now and permit a later replay.
			return nil, fmt.Errorf("auth: ws ticket single-use check unavailable: %w", err)
		}
		if !added {
			return nil, errors.New("auth: ws ticket already used")
		}
	}
	return claims, nil
}

// IssueSSOTicket creates a short-lived, single-use ticket that carries an
// authenticated SSO identity from the OIDC callback redirect to the SPA,
// which exchanges it for a real token pair. This keeps access/refresh tokens
// out of redirect URLs entirely.
func (m *Manager) IssueSSOTicket(userID, email string, role Role) (string, error) {
	now := time.Now()
	claims := Claims{
		UserID: userID,
		Email:  email,
		Role:   role,
		RegisteredClaims: jwt.RegisteredClaims{
			ID:        uuid.NewString(),
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(ssoTicketTTL)),
			Subject:   userID,
			Issuer:    m.issuer,
			Audience:  jwt.ClaimStrings{ssoTicketAudience},
		},
	}
	ticket, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(m.secret)
	if err != nil {
		return "", fmt.Errorf("auth: sign sso ticket: %w", err)
	}
	return ticket, nil
}

// ConsumeSSOTicket verifies an SSO exchange ticket and immediately revokes its
// jti via the blacklist, making the ticket single-use. A second exchange with
// the same ticket fails.
func (m *Manager) ConsumeSSOTicket(tokenStr string) (*Claims, error) {
	token, err := jwt.ParseWithClaims(tokenStr, &Claims{}, m.keyFunc,
		jwt.WithAudience(ssoTicketAudience), jwt.WithIssuer(m.issuer))
	if err != nil {
		return nil, fmt.Errorf("auth: verify sso ticket: %w", err)
	}
	claims, ok := token.Claims.(*Claims)
	if !ok || !token.Valid {
		return nil, errors.New("auth: invalid sso ticket claims")
	}
	if m.blacklist != nil && claims.ID != "" {
		ttl := time.Until(claims.ExpiresAt.Time)
		if ttl <= 0 {
			return nil, errors.New("auth: sso ticket already used")
		}
		added, err := m.blacklist.AddIfAbsent(claims.ID, ttl)
		if err != nil {
			return nil, fmt.Errorf("auth: sso ticket single-use check unavailable: %w", err)
		}
		if !added {
			return nil, errors.New("auth: sso ticket already used")
		}
	}
	return claims, nil
}

// Verify parses and validates an access token, returning its claims. It
// enforces the client audience so a WebSocket ticket cannot be used as an
// access token for ordinary API calls.
func (m *Manager) Verify(tokenStr string) (*Claims, error) {
	token, err := jwt.ParseWithClaims(tokenStr, &Claims{}, m.keyFunc,
		jwt.WithAudience(m.audience), jwt.WithIssuer(m.issuer))
	if err != nil {
		return nil, fmt.Errorf("auth: verify token: %w", err)
	}

	claims, ok := token.Claims.(*Claims)
	if !ok || !token.Valid {
		return nil, errors.New("auth: invalid token claims")
	}

	if m.blacklist != nil && claims.ID != "" && m.blacklist.IsRevoked(claims.ID) {
		return nil, errors.New("auth: token revoked")
	}

	return claims, nil
}

// VerifyIgnoreExpiry parses and validates an access token, returning its claims
// even if the token has expired. It still verifies the cryptographic signature,
// the audience, and the issuer.
//
// The audience/issuer re-check below is load-bearing, not belt-and-braces.
// jwt/v5 JOINS all claim-validation failures into one error, so
// errors.Is(err, ErrTokenExpired) reports true when the token is expired AND
// its audience is wrong. Tolerating expiry by that test alone therefore
// tolerated audience confusion: an expired WS ticket, SSO ticket, or refresh
// token — all signed with the same secret — was accepted here as an access
// token, claims and role included. Verified with a hand-signed expired
// wsTicketAudience token; see TestVerifyIgnoreExpiry_RejectsWrongAudience.
//
// Both current callers only feed the result to Revoke (de-privileging, and
// already behind an authenticated route), so this was not exploitable — but it
// is exactly the primitive a future caller would misuse, and the doc comment
// promised a guarantee the code did not deliver.
func (m *Manager) VerifyIgnoreExpiry(tokenStr string) (*Claims, error) {
	token, err := jwt.ParseWithClaims(tokenStr, &Claims{}, m.keyFunc,
		jwt.WithAudience(m.audience), jwt.WithIssuer(m.issuer))

	if err != nil && !errors.Is(err, jwt.ErrTokenExpired) {
		return nil, fmt.Errorf("auth: verify token signature: %w", err)
	}

	claims, ok := token.Claims.(*Claims)
	if !ok {
		return nil, errors.New("auth: invalid token claims")
	}

	// Re-assert the two claims the joined-error path above can swallow.
	if aud, aerr := claims.GetAudience(); aerr != nil || !slices.Contains(aud, m.audience) {
		return nil, errors.New("auth: token audience mismatch")
	}
	if iss, ierr := claims.GetIssuer(); ierr != nil || iss != m.issuer {
		return nil, errors.New("auth: token issuer mismatch")
	}

	if m.blacklist != nil && claims.ID != "" && m.blacklist.IsRevoked(claims.ID) {
		return nil, errors.New("auth: token revoked")
	}

	return claims, nil
}

// Revoke adds the token's JTI to the blacklist so it cannot be used again.
// If the Manager has no blacklist or the claims carry no ID this is a no-op.
func (m *Manager) Revoke(claims *Claims) {
	if m.blacklist == nil || claims.ID == "" {
		return
	}
	ttl := time.Until(claims.ExpiresAt.Time)
	if ttl <= 0 {
		return
	}
	m.blacklist.Add(claims.ID, ttl)
}

// RevokeJTI blacklists an arbitrary JTI for the given TTL, without needing a
// Claims struct. It is used to revoke personal access tokens: a PAT is not a
// JWT (it has no Claims), so Revoke cannot be used. The TTL should cover at
// least one cycle of the WS re-authz loop (2 min) so a live socket authenticated
// by the revoked credential is disconnected before the entry expires.
func (m *Manager) RevokeJTI(jti string, ttl time.Duration) {
	if m.blacklist == nil || jti == "" || ttl <= 0 {
		return
	}
	m.blacklist.Add(jti, ttl)
}

// PATJTI derives a stable, revocable JTI from a personal access token's id.
// PATs are not JWTs (they carry no JTI), so without a synthetic one a
// WebSocket ticket issued from a PAT-authenticated request embeds an empty
// SrcJTI — the WS re-authz loop then calls IsRevoked("") which is always false,
// so a revoked PAT's live socket is never disconnected. The "pat:" prefix
// keeps it out of the JWT-JTI namespace. Used both when authenticating a PAT
// (to populate the claims the WS ticket embeds) and when deleting a PAT (to
// blacklist the same id).
func PATJTI(tokenID string) string { return "pat:" + tokenID }

// ClaimsForPAT builds the Claims for a personal access token, carrying a stable
// JTI (PATJTI) and the token's expiry. This lets a WebSocket ticket issued from
// a PAT-authenticated request embed a real SrcJTI/SrcExp so the live socket is
// disconnectable on PAT revocation and respects the PAT's expiry.
func ClaimsForPAT(userID, email string, role Role, tokenID string, expiresAt *time.Time, scopes []string) *Claims {
	c := &Claims{UserID: userID, Email: email, Role: role, Scopes: scopes}
	c.ID = PATJTI(tokenID)
	if expiresAt != nil {
		c.ExpiresAt = jwt.NewNumericDate(*expiresAt)
	}
	return c
}

// IsRevoked returns true if the given JTI has been blacklisted. Today only
// logout (and any explicit access-token Revoke) blacklists an access JTI;
// password change and refresh-replay revoke refresh tokens, not access tokens.
// Used by long-lived connections (SSE, WebSocket) to periodically re-validate
// after the initial middleware check.
func (m *Manager) IsRevoked(jti string) bool {
	if m.blacklist == nil || jti == "" {
		return false
	}
	return m.blacklist.IsRevoked(jti)
}

// VerifyRefresh parses and validates a refresh token. It enforces the refresh
// audience so an access token cannot be presented at /auth/refresh (and a
// refresh token cannot be presented as an access token to Verify).
func (m *Manager) VerifyRefresh(tokenStr string) (*RefreshClaims, error) {
	token, err := jwt.ParseWithClaims(tokenStr, &RefreshClaims{}, m.keyFunc,
		jwt.WithAudience(refreshAudience), jwt.WithIssuer(m.issuer))
	if err != nil {
		return nil, fmt.Errorf("auth: verify refresh token: %w", err)
	}

	claims, ok := token.Claims.(*RefreshClaims)
	if !ok || !token.Valid {
		return nil, errors.New("auth: invalid refresh token claims")
	}

	return claims, nil
}

func (m *Manager) keyFunc(token *jwt.Token) (any, error) {
	if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
		return nil, fmt.Errorf("auth: unexpected signing method: %v", token.Header["alg"])
	}
	return m.secret, nil
}

// bcryptCost trades hash strength against login/registration latency: cost 12
// keeps hashing in the tens of milliseconds on typical server hardware while
// staying above the OWASP minimum of 10. Existing hashes created at a higher
// cost keep verifying (bcrypt embeds the cost in the hash).
const bcryptCost = 12

// HashPassword returns the bcrypt hash of the password.
func HashPassword(password string) (string, error) {
	bytes, err := bcrypt.GenerateFromPassword([]byte(password), bcryptCost)
	return string(bytes), err
}

// CheckPasswordHash compares a bcrypt hashed password with its possible
// plaintext equivalent. Returns true if it's a match.
func CheckPasswordHash(password, hash string) bool {
	err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(password))
	return err == nil
}
