package interfaces

import (
	"context"
	"errors"
	"time"
)

// ErrSecretSealingUnavailable is returned by a write of a secret that must be encrypted at rest when the backend has
// no field encryption key: the secret is refused rather than stored in plaintext.
var ErrSecretSealingUnavailable = errors.New("secret columns cannot be encrypted: no encryption key is configured (set PAD_ENCRYPTION_KEY)")

// ErrCICredentialLimit is returned by CreateCICredential when the organisation already holds the maximum number of
// unexpired CI credentials.
var ErrCICredentialLimit = errors.New("the organisation already has the maximum number of active CI credentials")

// CICredential is an organisation's CI credential (ADR 0019): a server-minted HMAC key a CI runner signs
// POST /api/integrations/ci with. ID is the public key id; Secret is the whole credential string, the HMAC key,
// encrypted at rest and never serialized.
type CICredential struct {
	ID         string     `json:"id"`
	OrgID      string     `json:"orgId"`
	Name       string     `json:"name"`
	Secret     string     `json:"-"`
	SecretHint string     `json:"secretHint"`
	CreatedBy  string     `json:"createdBy"`
	CreatedAt  time.Time  `json:"createdAt"`
	ExpiresAt  *time.Time `json:"expiresAt,omitempty"` // nil: never expires
	LastUsedAt *time.Time `json:"lastUsedAt,omitempty"`
}

// Active reports whether the credential has not expired at now.
func (c *CICredential) Active(now time.Time) bool {
	return c.ExpiresAt == nil || c.ExpiresAt.After(now)
}

// CICredentialStore holds the organisations' CI credentials (org_ci_credentials, migration 0038) and the org-scoped
// flow read the CI webhook resolves its flow with. The webhook runs outside any request scope, on the pool, so the
// organisation is an explicit predicate of every statement it issues (ADR 0013).
type CICredentialStore interface {
	// CreateCICredential stores c (Secret sealed at rest; CreatedAt stamped when zero) unless c.OrgID already holds
	// maxActive unexpired credentials (ErrCICredentialLimit). Without field encryption it stores nothing and returns
	// ErrSecretSealingUnavailable.
	CreateCICredential(ctx context.Context, c *CICredential, maxActive int) error
	// GetCICredential returns the credential with key id id, Secret opened, or ErrNotFound.
	GetCICredential(ctx context.Context, id string) (*CICredential, error)
	// ListCICredentials returns orgID's credentials, expired ones included, newest first, without their secrets.
	ListCICredentials(ctx context.Context, orgID string) ([]*CICredential, error)
	// ActiveCICredentials returns up to limit of orgID's unexpired credentials, Secret opened, the most recently used
	// first.
	ActiveCICredentials(ctx context.Context, orgID string, limit int) ([]*CICredential, error)
	// DeleteCICredential deletes orgID's credential id; ErrNotFound when orgID has no such credential.
	DeleteCICredential(ctx context.Context, orgID, id string) error
	// TouchCICredential sets last_used_at to now unless it was set in the last five minutes; a missing credential is
	// not an error.
	TouchCICredential(ctx context.Context, id string) error
	// LoadFlowHeaderInOrg is LoadFlowHeader for a flow of orgID only: ErrNotFound for a flow of another
	// organisation, a personal flow, or none.
	LoadFlowHeaderInOrg(ctx context.Context, id, orgID string) (*FlowDocument, error)
	// RecordCIDelivery records that the webhook's caller (the key that authenticated a call, as the CI service names
	// it) sent deliveryID (ci_webhook_deliveries, migration 0039). It reports false, recording nothing, when the pair
	// is already recorded: a replay. Of concurrent records of one pair exactly one reports true.
	RecordCIDelivery(ctx context.Context, caller, deliveryID string) (bool, error)
}
