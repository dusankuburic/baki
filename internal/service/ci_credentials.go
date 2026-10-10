package service

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/lifecycle"
	"pad-analyzer/internal/platform/apperr"
	"pad-analyzer/internal/platform/hmacsig"
	"pad-analyzer/internal/platform/secret"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-core/logger"
	"pad-core/models"
)

// Organisation CI credentials (R8, ADR 0019). An org admin mints a credential, baki_ci_<keyId>_<secret>, and hands it
// to a CI runner, which signs POST /api/integrations/ci with the WHOLE string as the HMAC key: the signature format is
// the one the deployment-wide PAD_CI_WEBHOOK_SECRET always used, so a bakicli release that predates the credentials
// signs correctly with one. The key id is public (the X-Baki-Key-Id header, the primary key); the secret is stored
// sealed and shown once. A credential reaches its own organisation's flows only.

const (
	// CICredentialPrefix starts every organisation CI credential.
	CICredentialPrefix = "baki_ci_"
	// ciKeyIDLength is the key id's length: 10 random bytes in unpadded lowercase base32.
	ciKeyIDLength = 16
	// MaxActiveCICredentials is how many unexpired credentials an organisation may hold, and so how many the
	// fallback for old clients verifies at most.
	MaxActiveCICredentials = 10
	// MaxCICredentialDays is the longest lifetime an admin may give a credential; none means it never expires.
	MaxCICredentialDays = 365
	// maxCICredentialName bounds a credential's name, in characters.
	maxCICredentialName = 100
	// maxConcurrentCITouches caps the detached last-used writes in flight; past it a write is dropped and the next
	// call records the use.
	maxConcurrentCITouches = 8
	// ciTouchTimeout bounds one detached last-used write.
	ciTouchTimeout = 5 * time.Second
	// minCIDeliveryID and maxCIDeliveryID bound a delivery id: bakicli sends 128 random bits as 32 hex characters.
	// The floor keeps a sloppy client's ids from colliding with its own (each a false replay); the ceiling bounds
	// the row (the table's check constraint agrees).
	minCIDeliveryID = 16
	maxCIDeliveryID = 128
)

var ciKeyIDEncoding = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

var (
	// ErrCIUnauthenticated is every way a CI webhook call fails to authenticate, with one answer: an unknown key id,
	// an unknown or personal flow, an organisation without credentials, an expired credential, a bad signature and a
	// stale timestamp are not told apart.
	ErrCIUnauthenticated = apperr.Unauthenticated("", "invalid or missing CI webhook signature")
	// ErrCIMissingTimestamp is a call signed without X-Baki-Timestamp, which only the deprecated deployment secret
	// may still accept (PAD_CI_WEBHOOK_ALLOW_NO_TIMESTAMP=1).
	ErrCIMissingTimestamp = apperr.Unauthenticated("", "missing X-Baki-Timestamp (replay protection): sign \"<timestamp>.<body>\" and send the timestamp, as bakicli report does")
	// ErrCITokenUnauthenticated is every way a bearer token fails to authenticate a webhook call, with one answer:
	// no API token, an unknown, revoked or expired one, and one without the ci scope, an unscoped one included.
	ErrCITokenUnauthenticated = apperr.Unauthenticated("", "the CI webhook takes an API token with the ci scope (Authorization: Bearer pad_pat_…)")
	// ErrCIReplayed is a call whose deliveryId its key has sent before: that call ran, this one does not.
	ErrCIReplayed = apperr.Conflict("CI_REPLAYED", "this deliveryId was already received from this key: the call it came with ran, and a replay does not run; send a new deliveryId for a new run")

	errCIDeliveryInvalid = apperr.Invalid("", fmt.Sprintf("deliveryId must be %d to %d characters of A-Z, a-z, 0-9, '-' and '_'", minCIDeliveryID, maxCIDeliveryID))

	errCICredentialLimit = apperr.Conflict("CI_CREDENTIAL_LIMIT",
		fmt.Sprintf("an organisation can hold at most %d active CI credentials: revoke one first", MaxActiveCICredentials))
	errCIEncryptionOff = apperr.Unavailable("CI_CREDENTIALS_UNAVAILABLE",
		"CI credentials need at-rest encryption: set PAD_ENCRYPTION_KEY on the server")
	errCIStoreMissing      = apperr.Unavailable("CI_CREDENTIALS_UNAVAILABLE", "CI credentials need the server store")
	errCICredentialMissing = apperr.NotFound("", "no such CI credential in this organisation")
)

// ciStore is what the CI service reads and writes: the credentials and the flows they analyse.
type ciStore interface {
	storageif.CICredentialStore
	LoadFlowHeader(ctx context.Context, id string) (*storageif.FlowDocument, error)
	LoadFlow(ctx context.Context, id string) (*storageif.FlowDocument, error)
}

// CIOptions configures the CI service from the deployment.
type CIOptions struct {
	// GlobalSecret is PAD_CI_WEBHOOK_SECRET, the deployment-wide key; unset for none.
	GlobalSecret secret.Value
	// AllowNoTimestamp lets GlobalSecret verify the legacy signature over the body alone.
	AllowNoTimestamp bool
	// OrgCredentials: the deployment has accounts, so organisations exist and mint their own credentials. The
	// global secret is then deprecated.
	OrgCredentials bool
}

// CIService authenticates CI webhook calls, resolves the flow a call may analyse, and manages the organisations' CI
// credentials.
type CIService struct {
	store   ciStore
	docs    DocumentProvider
	authz   *AuthzService
	opts    CIOptions
	touches *lifecycle.Bounded
	now     func() time.Time
}

// NewCIService builds the CI service. store is nil without the server store (the desktop), where the webhook and
// the credential routes are not served. docs resolves a stored flow's document (the cloud provider caches it).
// authz decides which flows a personal API token's user may run the webhook on; without it a token reaches none.
func NewCIService(store storageif.StorageBackend, docs DocumentProvider, authz *AuthzService, opts CIOptions) *CIService {
	s := &CIService{docs: docs, authz: authz, opts: opts, touches: lifecycle.NewBounded("ci_credential_touch", maxConcurrentCITouches), now: time.Now}
	if store != nil {
		s.store = store
	}
	return s
}

// Configured reports whether a webhook call can authenticate at all: with organisation credentials, or with the
// deployment's secret.
func (s *CIService) Configured() bool {
	return s != nil && s.store != nil && (s.opts.OrgCredentials || s.opts.GlobalSecret.IsSet())
}

// CIRequest is what a webhook call presents: its headers and the raw body it signed.
type CIRequest struct {
	KeyID     string // X-Baki-Key-Id
	Timestamp string // X-Baki-Timestamp
	Signature string // X-Baki-Signature
	Body      []byte
}

// CICaller is who an authenticated webhook call is.
type CICaller struct {
	// Method is how it authenticated: CIAuthKeyID, CIAuthGlobal, CIAuthFlowOrg or CIAuthToken.
	Method string
	// OrgID and CredentialID name the organisation CI credential; both "" for the deployment's secret and a token.
	OrgID        string
	CredentialID string
	// Deprecated: the deployment's secret, on a deployment where organisations have credentials of their own.
	Deprecated bool
	// token is the personal API token of a CIAuthToken call; nil for the HMAC methods.
	token *auth.Actor
}

// Actor is the caller as authorization and audit see it: the token's user for a personal API token, otherwise an
// integration, never a user.
func (c *CICaller) Actor() *auth.Actor {
	if c.token != nil {
		return c.token
	}
	return auth.IntegrationActor(c.OrgID, c.CredentialID)
}

// deliveryKey names the key that authenticated the call in the replay record: the organisation credential (by key
// id or through the flow alike), the deployment's secret, or the API token.
func (c *CICaller) deliveryKey() string {
	switch {
	case c.token != nil:
		return "token:" + c.token.TokenID
	case c.CredentialID != "":
		return "credential:" + c.CredentialID
	}
	return "secret"
}

// Authenticate verifies a webhook call. In order: the organisation credential its X-Baki-Key-Id names; the
// deployment's secret; and, for a call without a key id (a bakicli release that predates them), the credentials of
// the organisation owning the flow the body names. Every failure is ErrCIUnauthenticated (ErrCIMissingTimestamp for
// a call without a timestamp); any other error is a store failure.
func (s *CIService) Authenticate(ctx context.Context, req CIRequest) (*CICaller, error) {
	if !s.Configured() || req.Signature == "" {
		return nil, ErrCIUnauthenticated
	}
	if req.Timestamp == "" {
		// Only the deployment's secret ever accepted the untimed form, and only when the operator allows it.
		if !s.opts.GlobalSecret.IsSet() || !s.opts.AllowNoTimestamp {
			return nil, ErrCIMissingTimestamp
		}
		if !hmacsig.VerifyUntimed([]byte(s.opts.GlobalSecret.Reveal()), req.Body, req.Signature) {
			return nil, ErrCIUnauthenticated
		}
		logger.FromContext(ctx).Warn("CI webhook accepted WITHOUT X-Baki-Timestamp (PAD_CI_WEBHOOK_ALLOW_NO_TIMESTAMP=1; replay-vulnerable): update the sender")
		return s.accept(ctx, CIAuthGlobal, nil), nil
	}
	ts, fresh := hmacsig.Timestamp(req.Timestamp, s.now())
	if !fresh {
		return nil, ErrCIUnauthenticated
	}
	if req.KeyID != "" && s.opts.OrgCredentials {
		c, err := s.credentialByKeyID(ctx, req.KeyID)
		if err != nil {
			return nil, err
		}
		if c != nil && hmacsig.Verify([]byte(c.Secret), ts, req.Body, req.Signature) {
			return s.accept(ctx, CIAuthKeyID, c), nil
		}
	}
	if s.opts.GlobalSecret.IsSet() && hmacsig.Verify([]byte(s.opts.GlobalSecret.Reveal()), ts, req.Body, req.Signature) {
		return s.accept(ctx, CIAuthGlobal, nil), nil
	}
	if req.KeyID == "" && s.opts.OrgCredentials {
		c, err := s.credentialByFlow(ctx, ts, req)
		if err != nil {
			return nil, err
		}
		if c != nil {
			logger.FromContext(ctx).Debug("CI webhook authenticated through the flow's organisation (a client without X-Baki-Key-Id)", "keyId", c.ID)
			return s.accept(ctx, CIAuthFlowOrg, c), nil
		}
	}
	return nil, ErrCIUnauthenticated
}

// AuthenticateToken admits a webhook call made with a personal API token, the actor the API layer verified the
// bearer as (nil when it is no live token). Only a token carrying the ci scope is admitted: an unscoped token,
// which may do everything else its user can, is refused here, so a token minted before the scope existed never
// starts running CI. Tokens exist only with accounts. Every refusal is ErrCITokenUnauthenticated.
func (s *CIService) AuthenticateToken(actor *auth.Actor) (*CICaller, error) {
	if !s.Configured() || !s.opts.OrgCredentials || actor == nil || actor.Kind != auth.ActorToken || actor.TokenID == "" ||
		!actor.Scoped() || !auth.HasScope(actor.Scopes, auth.ScopeCI) {
		return nil, ErrCITokenUnauthenticated
	}
	recordCIWebhookAuth(CIAuthToken)
	return &CICaller{Method: CIAuthToken, token: actor}, nil
}

// ClaimDelivery records the delivery id an authenticated call carries, for its key, before the call runs: a
// captured call replayed inside the timestamp window, or a bearer call resent, is ErrCIReplayed, and of concurrent
// calls with one id exactly one goes on. A call without one (a bakicli release before it) goes on, counted; a
// malformed one is a 400. Any other error is a store failure, and the call does not run.
func (s *CIService) ClaimDelivery(ctx context.Context, caller *CICaller, deliveryID string) error {
	if deliveryID == "" {
		recordCIWebhookDelivery(CIDeliveryMissing)
		return nil
	}
	if !validCIDeliveryID(deliveryID) {
		return errCIDeliveryInvalid
	}
	if s.store == nil {
		return errCIStoreMissing
	}
	fresh, err := s.store.RecordCIDelivery(ctx, caller.deliveryKey(), deliveryID)
	if err != nil {
		return fmt.Errorf("ci webhook: record delivery: %w", err)
	}
	if !fresh {
		recordCIWebhookDelivery(CIDeliveryReplayed)
		return ErrCIReplayed
	}
	recordCIWebhookDelivery(CIDeliveryNew)
	return nil
}

// validCIDeliveryID reports whether id is minCIDeliveryID to maxCIDeliveryID characters of the URL-safe alphabet a
// random id is written in (hex, base64url, a UUID).
func validCIDeliveryID(id string) bool {
	if len(id) < minCIDeliveryID || len(id) > maxCIDeliveryID {
		return false
	}
	for _, r := range id {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '-' && r != '_' {
			return false
		}
	}
	return true
}

// credentialByKeyID returns the unexpired credential with key id, nil when there is none.
func (s *CIService) credentialByKeyID(ctx context.Context, keyID string) (*storageif.CICredential, error) {
	if !validCIKeyID(keyID) {
		return nil, nil
	}
	c, err := s.store.GetCICredential(ctx, keyID)
	if errors.Is(err, storageif.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("ci webhook: read credential: %w", err)
	}
	if !c.Active(s.now()) {
		return nil, nil
	}
	return c, nil
}

// maxFallbackBody bounds the calls the old-client fallback considers: bakicli's bodies are under 200 bytes, and an
// unauthenticated caller must not make the server HMAC a large body once per credential of an organisation.
const maxFallbackBody = 4 << 10

// credentialByFlow is the fallback for a call without a key id: the organisation owning the flow the body names, and
// the first of its unexpired credentials that verifies the call. The body is already bounded and is parsed only
// for the flow id. Nothing has been authenticated yet, so every failure, a storage error included, is "no
// credential" (the caller's 401, never a 500 or an ERROR log), and a flow id that cannot be one is not looked up.
func (s *CIService) credentialByFlow(ctx context.Context, ts int64, req CIRequest) (*storageif.CICredential, error) {
	if len(req.Body) > maxFallbackBody {
		return nil, nil
	}
	var body struct {
		FlowID string `json:"flowId"`
	}
	if json.Unmarshal(req.Body, &body) != nil || !plausibleFlowID(body.FlowID) {
		return nil, nil
	}
	h, err := s.store.LoadFlowHeader(ctx, body.FlowID)
	if err != nil {
		if !errors.Is(err, storageif.ErrNotFound) {
			logger.FromContext(ctx).Warn("ci webhook: fallback flow read failed", "err", err)
		}
		return nil, nil
	}
	if h.OrganizationID == "" {
		return nil, nil
	}
	creds, err := s.store.ActiveCICredentials(ctx, h.OrganizationID, MaxActiveCICredentials)
	if err != nil {
		logger.FromContext(ctx).Warn("ci webhook: fallback credential read failed", "err", err)
		return nil, nil
	}
	for _, c := range creds {
		if hmacsig.Verify([]byte(c.Secret), ts, req.Body, req.Signature) {
			return c, nil
		}
	}
	return nil, nil
}

// plausibleFlowID reports whether id can name a flow: non-empty, at most 128 bytes, valid UTF-8 without control
// characters (Postgres refuses a NUL in a query parameter).
func plausibleFlowID(id string) bool {
	if id == "" || len(id) > 128 || !utf8.ValidString(id) {
		return false
	}
	for _, r := range id {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// accept counts an authenticated call and records the credential's use off the response path.
func (s *CIService) accept(ctx context.Context, method string, c *storageif.CICredential) *CICaller {
	recordCIWebhookAuth(method)
	if c == nil {
		return &CICaller{Method: method, Deprecated: s.opts.OrgCredentials}
	}
	detached := context.WithoutCancel(ctx)
	id := c.ID
	s.touches.Go(func() {
		ctx, cancel := context.WithTimeout(detached, ciTouchTimeout)
		defer cancel()
		if err := s.store.TouchCICredential(ctx, id); err != nil {
			logger.FromContext(ctx).Warn("ci credential: recording its use failed", "keyId", id, "err", err)
		}
	})
	return &CICaller{Method: method, OrgID: c.OrgID, CredentialID: c.ID}
}

// ResolveFlow resolves the flow an authenticated call names: an organisation credential reaches its organisation's
// flows only (an explicit predicate, as the call runs outside any user's row-level security); a personal API token
// reaches the flows its user may read, an organisation's only as its member (AuthzService, CIRun); the deployment's
// secret reaches any flow by id. Another organisation's flow, a personal one and an unknown one are all
// storageif.ErrNotFound for a credential; for a token, a flow it may not run is ErrPermissionDenied, which the
// webhook answers like an unknown one.
func (s *CIService) ResolveFlow(ctx context.Context, caller *CICaller, flowID string) (*models.FlowDocument, error) {
	if s.store == nil {
		return nil, errCIStoreMissing
	}
	var header *storageif.FlowDocument
	var err error
	switch {
	case caller.token != nil:
		if s.authz == nil {
			return nil, ErrPermissionDenied
		}
		header, err = s.authz.AuthorizeFlow(ctx, caller.token, CIRun, flowID)
	case caller.OrgID != "":
		header, err = s.store.LoadFlowHeaderInOrg(ctx, flowID, caller.OrgID)
	default:
		header, err = s.store.LoadFlowHeader(ctx, flowID)
	}
	if err != nil {
		return nil, err
	}
	var doc *models.FlowDocument
	if hr, ok := s.docs.(headerDocResolver); ok {
		doc, err = hr.ResolveDocWithHeader(ctx, flowID, header)
	} else {
		// Local mode's provider holds the open document only; the webhook names a stored flow.
		var stored *storageif.FlowDocument
		if stored, err = s.store.LoadFlow(ctx, flowID); err == nil {
			doc, err = resolveStoredFlow(flowID, stored)
		}
	}
	if err != nil {
		return nil, err
	}
	if caller.OrgID != "" && doc.OrganizationID != caller.OrgID {
		// The flow changed organisation between the two reads.
		return nil, storageif.ErrNotFound
	}
	return doc, nil
}

// ListCredentials lists an organisation's CI credentials, without their secrets.
func (s *CIService) ListCredentials(ctx context.Context, orgID string) ([]*storageif.CICredential, error) {
	if s.store == nil {
		return nil, errCIStoreMissing
	}
	return s.store.ListCICredentials(ctx, orgID)
}

// CreateCredential mints a CI credential for orgID, named name, created by createdBy, expiring after expiresInDays
// days (0: never). It returns the stored credential and the credential string, which is never readable again.
func (s *CIService) CreateCredential(ctx context.Context, orgID, createdBy, name string, expiresInDays int) (*storageif.CICredential, string, error) {
	if s.store == nil {
		return nil, "", errCIStoreMissing
	}
	name = strings.TrimSpace(name)
	switch {
	case name == "":
		return nil, "", apperr.Invalid("", "name is required")
	case utf8.RuneCountInString(name) > maxCICredentialName:
		return nil, "", apperr.Invalid("", fmt.Sprintf("name is longer than %d characters", maxCICredentialName))
	case expiresInDays < 0 || expiresInDays > MaxCICredentialDays:
		return nil, "", apperr.Invalid("", fmt.Sprintf("expiresInDays must be between 1 and %d, or left out for a credential that never expires", MaxCICredentialDays))
	}
	keyID, credential, err := newCICredential()
	if err != nil {
		return nil, "", err
	}
	c := &storageif.CICredential{
		ID: keyID, OrgID: orgID, Name: name, Secret: credential, SecretHint: credential[len(credential)-4:],
		CreatedBy: createdBy, CreatedAt: s.now().UTC().Truncate(time.Microsecond),
	}
	if expiresInDays > 0 {
		exp := c.CreatedAt.AddDate(0, 0, expiresInDays)
		c.ExpiresAt = &exp
	}
	switch err := s.store.CreateCICredential(ctx, c, MaxActiveCICredentials); {
	case errors.Is(err, storageif.ErrCICredentialLimit):
		return nil, "", errCICredentialLimit
	case errors.Is(err, storageif.ErrSecretSealingUnavailable):
		return nil, "", errCIEncryptionOff
	case err != nil:
		return nil, "", err
	}
	out := *c
	out.Secret = ""
	return &out, credential, nil
}

// RevokeCredential deletes orgID's credential keyID; a runner signing with it gets 401 from the next call on.
func (s *CIService) RevokeCredential(ctx context.Context, orgID, keyID string) error {
	if s.store == nil {
		return errCIStoreMissing
	}
	if err := s.store.DeleteCICredential(ctx, orgID, keyID); errors.Is(err, storageif.ErrNotFound) {
		return errCICredentialMissing
	} else if err != nil {
		return err
	}
	return nil
}

// newCICredential mints a key id and the credential string around it.
func newCICredential() (keyID, credential string, err error) {
	id := make([]byte, 10)
	secret := make([]byte, 32)
	if _, err := rand.Read(id); err != nil {
		return "", "", fmt.Errorf("ci credential: %w", err)
	}
	if _, err := rand.Read(secret); err != nil {
		return "", "", fmt.Errorf("ci credential: %w", err)
	}
	keyID = ciKeyIDEncoding.EncodeToString(id)
	return keyID, CICredentialPrefix + keyID + "_" + base64.RawURLEncoding.EncodeToString(secret), nil
}

// CIKeyID returns the key id of a CI credential string, "" when it is not one (a deployment secret, say).
func CIKeyID(credential string) string {
	rest, ok := strings.CutPrefix(credential, CICredentialPrefix)
	if !ok || len(rest) <= ciKeyIDLength || rest[ciKeyIDLength] != '_' || !validCIKeyID(rest[:ciKeyIDLength]) {
		return ""
	}
	return rest[:ciKeyIDLength]
}

// validCIKeyID reports whether id has the shape of a key id, so a malformed header costs no lookup.
func validCIKeyID(id string) bool {
	if len(id) != ciKeyIDLength {
		return false
	}
	for _, r := range id {
		if (r < 'a' || r > 'z') && (r < '2' || r > '7') {
			return false
		}
	}
	return true
}
