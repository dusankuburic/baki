package auth

import (
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

// Claims are the custom JWT claims stored in every access token.
type Claims struct {
	UserID string `json:"uid"`
	Email  string `json:"email"`
	Role   Role   `json:"role"`
	jwt.RegisteredClaims
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
	AccessToken  string
	RefreshToken string
	ExpiresAt    time.Time
	// RefreshID is the refresh token's unique ID (jti). It lets a server-side
	// store track and revoke individual refresh tokens for rotation.
	RefreshID        string
	RefreshExpiresAt time.Time
}

const (
	defaultAccessTTL  = 15 * time.Minute
	defaultRefreshTTL = 7 * 24 * time.Hour
	// wsTicketTTL bounds how long a WebSocket connect ticket is valid. Tickets
	// are exchanged for a live connection within seconds of issuance, so the
	// window is deliberately tiny to limit replay if one leaks (e.g. via a
	// proxy access log that records the ?ticket= query parameter).
	wsTicketTTL = 30 * time.Second
	// wsTicketAudience tags tickets so they cannot be used as API access tokens
	// (and access tokens cannot be used as tickets).
	wsTicketAudience = "pad-ws-ticket"
)

// Manager handles JWT issuance and verification.
type Manager struct {
	secret     []byte
	accessTTL  time.Duration
	refreshTTL time.Duration
	issuer     string
	audience   string
}

// NewManager creates a Manager using the provided HMAC-SHA256 secret.
func NewManager(secret string) *Manager {
	return NewManagerWithTTL(secret, defaultAccessTTL, defaultRefreshTTL, "pad-analyzer", "pad-client")
}

// NewManagerWithTTL creates a Manager with explicit TTL values (useful for tests).
func NewManagerWithTTL(secret string, accessTTL, refreshTTL time.Duration, issuer, audience string) *Manager {
	return &Manager{
		secret:     []byte(secret),
		accessTTL:  accessTTL,
		refreshTTL: refreshTTL,
		issuer:     issuer,
		audience:   audience,
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
			Audience:  jwt.ClaimStrings{m.audience},
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
// Returns the signed ticket and its expiry.
func (m *Manager) IssueWSTicket(userID, email string, role Role) (string, time.Time, error) {
	now := time.Now()
	expiresAt := now.Add(wsTicketTTL)
	claims := Claims{
		UserID: userID,
		Email:  email,
		Role:   role,
		RegisteredClaims: jwt.RegisteredClaims{
			ID:        uuid.NewString(),
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(expiresAt),
			Subject:   userID,
			Issuer:    m.issuer,
			Audience:  jwt.ClaimStrings{wsTicketAudience},
		},
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
		jwt.WithAudience(wsTicketAudience))
	if err != nil {
		return nil, fmt.Errorf("auth: verify ws ticket: %w", err)
	}
	claims, ok := token.Claims.(*Claims)
	if !ok || !token.Valid {
		return nil, errors.New("auth: invalid ws ticket claims")
	}
	return claims, nil
}

// Verify parses and validates an access token, returning its claims. It
// enforces the client audience so a WebSocket ticket cannot be used as an
// access token for ordinary API calls.
func (m *Manager) Verify(tokenStr string) (*Claims, error) {
	token, err := jwt.ParseWithClaims(tokenStr, &Claims{}, m.keyFunc,
		jwt.WithAudience(m.audience))
	if err != nil {
		return nil, fmt.Errorf("auth: verify token: %w", err)
	}

	claims, ok := token.Claims.(*Claims)
	if !ok || !token.Valid {
		return nil, errors.New("auth: invalid token claims")
	}

	return claims, nil
}

// VerifyIgnoreExpiry parses and validates an access token, returning its claims
// even if the token has expired. It still verifies the cryptographic signature
// and the audience.
func (m *Manager) VerifyIgnoreExpiry(tokenStr string) (*Claims, error) {
	token, err := jwt.ParseWithClaims(tokenStr, &Claims{}, m.keyFunc,
		jwt.WithAudience(m.audience))

	if err != nil && !errors.Is(err, jwt.ErrTokenExpired) {
		return nil, fmt.Errorf("auth: verify token signature: %w", err)
	}

	claims, ok := token.Claims.(*Claims)
	if !ok {
		return nil, errors.New("auth: invalid token claims")
	}

	return claims, nil
}

// VerifyRefresh parses and validates a refresh token.
func (m *Manager) VerifyRefresh(tokenStr string) (*RefreshClaims, error) {
	token, err := jwt.ParseWithClaims(tokenStr, &RefreshClaims{}, m.keyFunc)
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
