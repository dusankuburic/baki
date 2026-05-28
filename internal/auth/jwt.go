package auth

import (
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
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
}

const (
	defaultAccessTTL  = 15 * time.Minute
	defaultRefreshTTL = 7 * 24 * time.Hour
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

	refreshClaims := RefreshClaims{
		UserID: userID,
		Email:  email,
		Role:   role,
		RegisteredClaims: jwt.RegisteredClaims{
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(m.refreshTTL)),
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
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		ExpiresAt:    expiresAt,
	}, nil
}

// Verify parses and validates an access token, returning its claims.
func (m *Manager) Verify(tokenStr string) (*Claims, error) {
	token, err := jwt.ParseWithClaims(tokenStr, &Claims{}, m.keyFunc)
	if err != nil {
		return nil, fmt.Errorf("auth: verify token: %w", err)
	}

	claims, ok := token.Claims.(*Claims)
	if !ok || !token.Valid {
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
