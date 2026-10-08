package service

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/storage"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-core/logger"
)

// Azure AI Foundry credential helpers. Every resource's credential is one KeyStore document (the default
// resource's under the historical key "azure-foundry", others under "azure-foundry/<id>") for a user or an
// organisation. The single-document routes and the model registry share them: an omitted secret is carried by
// exactly the same rules wherever a document is saved or tested.

// storedAzureConfig reads and parses a stored document; hadPrev is false when there is none or it is unreadable.
func storedAzureConfig(get func() (string, error)) (prev ai.AzureFoundryConfig, hadPrev bool) {
	raw, err := get()
	if err != nil || raw == "" {
		return ai.AzureFoundryConfig{}, false
	}
	p, parseErr := ai.ParseAzureFoundryConfig(raw)
	if parseErr != nil {
		return ai.AzureFoundryConfig{}, false
	}
	return p, true
}

// carryAzureSecret keeps the stored secret for a save that omits it — an API key whenever the auth mode
// matches, an Entra ID client secret only for the same principal — and only while the document still sends it
// to the hosts it was entered for: endpoint host unchanged, and a project endpoint on the previous project's
// or endpoint's host. Otherwise anyone able to edit the document (another org admin, for a shared one) could
// point the stored key at another allow-listed host without knowing it. A refusal is a MISSING_SECRET error
// saying why; nil means carried, or nothing to carry.
func carryAzureSecret(cfg *ai.AzureFoundryConfig, prev ai.AzureFoundryConfig, hadPrev bool) error {
	if cfg.HasSecret() || !hadPrev || prev.Auth.Mode != cfg.Auth.Mode {
		return nil
	}
	switch cfg.Auth.Mode {
	case ai.AzureAuthModeAPIKey:
		// The resource key is not principal-bound.
	case ai.AzureAuthModeEntra:
		if prev.Auth.TenantID != cfg.Auth.TenantID || prev.Auth.ClientID != cfg.Auth.ClientID {
			return nil // another principal: its own secret is required
		}
	default:
		return nil
	}
	if !azureSecretHostsKept(prev, *cfg) {
		return &ai.AzureConfigError{
			Code:    ai.AzureCfgErrMissingSecret,
			Message: "the endpoint changed: re-enter the " + azureSecretName(cfg.Auth.Mode) + " — a stored secret is only kept for the endpoint it was entered for",
		}
	}
	if cfg.Auth.Mode == ai.AzureAuthModeEntra {
		cfg.Auth.ClientSecret = prev.Auth.ClientSecret
	} else {
		cfg.Auth.Key = prev.Auth.Key
	}
	return nil
}

// azureSecretName is how a save error names the secret a mode needs.
func azureSecretName(mode string) string {
	if mode == ai.AzureAuthModeEntra {
		return "client secret"
	}
	return "API key"
}

// azureSecretHostsKept reports whether next sends its credential only to hosts prev already sent it to: the
// same endpoint host, and no project endpoint or one on prev's project or endpoint host.
func azureSecretHostsKept(prev, next ai.AzureFoundryConfig) bool {
	prevHost, nextHost := azureURLHost(prev.Endpoint), azureURLHost(next.Endpoint)
	if nextHost == "" || nextHost != prevHost {
		return false
	}
	if next.ProjectEndpoint == "" {
		return true
	}
	project := azureURLHost(next.ProjectEndpoint)
	return project != "" && (project == prevHost || project == azureURLHost(prev.ProjectEndpoint))
}

// azureURLHost is the lower-cased host[:port] of an Azure URL, "" when it does not parse.
func azureURLHost(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Host)
}

// saveCarryError is what a save whose secret was not carried reports (under the document's own validation).
func saveCarryError(cfg ai.AzureFoundryConfig, refusal error) error {
	return saveCarryErrorWith(cfg, refusal, ai.ValidateAzureFoundryConfig)
}

// saveCarryErrorWith is what a save (or draft test) whose secret was not carried reports: a validation failure
// of the document itself wins over the refusal, which is only worth acting on once the rest is right.
// validate is the rule the caller saves under — the single document's or a registry resource's.
func saveCarryErrorWith(cfg ai.AzureFoundryConfig, refusal error, validate func(ai.AzureFoundryConfig) error) error {
	probe := cfg
	if probe.Auth.Mode == ai.AzureAuthModeEntra {
		probe.Auth.ClientSecret = "placeholder"
	} else {
		probe.Auth.Key = "placeholder"
	}
	if err := validate(probe); err != nil {
		return err
	}
	return refusal
}

// formatOrgKeyStamp renders an org credential's updated_at as the CAS token: RFC 3339 in UTC.
func formatOrgKeyStamp(at time.Time) string { return at.UTC().Format(time.RFC3339Nano) }

// parseOrgKeyStamp reads a save's updatedAt token; "" is the zero time.
func parseOrgKeyStamp(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, &ai.AzureConfigError{Code: ai.AzureCfgErrInvalidConfig, Message: "updatedAt must be the RFC 3339 timestamp the configuration was loaded with"}
	}
	return t, nil
}

// unreadableOrgStamp classifies the stored org document under key for a repair save: when a row exists but
// cannot be read (decrypt failure, or unparseable by this build) it reports the updated_at stamp OrgKeyMeta
// still carries — the CAS token that turns the replacement into a real CAS. found is false when there is
// nothing to repair (no row, a READABLE document, no meta stamp, or a store error the caller must see).
func unreadableOrgStamp(orgKeys OrgKeyStore, orgID, key string) (stamp time.Time, found bool, err error) {
	raw, getErr := orgKeys.GetOrg(orgID, key)
	switch {
	case errors.Is(getErr, storage.ErrSecretDecryptFailed):
		// A row exists but cannot be decrypted (key rotation).
	case getErr == nil && raw != "":
		if _, parseErr := ai.ParseAzureFoundryConfig(raw); parseErr == nil {
			return time.Time{}, false, nil // readable: the save carries its token
		}
	case getErr == nil, errors.Is(getErr, storage.ErrSecretNotFound):
		return time.Time{}, false, nil // empty document / no row: the insert path
	default:
		return time.Time{}, false, nil // e.g. a database failure: today's path
	}
	meta, ok, metaErr := orgKeys.OrgKeyMeta(orgID, key)
	if metaErr != nil {
		return time.Time{}, false, metaErr
	}
	if !ok || meta.UpdatedAt.IsZero() {
		return time.Time{}, false, nil
	}
	return meta.UpdatedAt, true, nil
}

// ---- Undoing a credential save whose registry write failed ----
//
// A resource's credential lives in the KeyStore, its row in the registry, with no transaction spanning the two:
// the credential is saved first, then the row is written. Every check the row write can fail on runs before the
// save, but a race or a store failure can still refuse it — and a failed request must not leave the new
// credential (for an organisation: a new lender) in effect. So the previous credential is snapshotted before
// the save and put back after a failure; when even that fails the error says the credential changed
// (AzureCredentialChangedError) and the handler audits it.

// azureCredentialSnapshot is one resource's stored credential as it was before a save.
type azureCredentialSnapshot struct {
	// exists: a credential was stored.
	exists bool
	// restorable: restoreCredential can put it back — no credential always (it deletes the new one), a readable
	// personal document, or an org row (OrgSecretRestorer: sealed document with writer, lender and stamp).
	restorable bool
	// raw is the personal document; org is the organisation's row as stored.
	raw string
	org storageif.OrgKeySnapshot
}

// errCredentialNotRestorable: the snapshot cannot be written back.
var errCredentialNotRestorable = errors.New("the previous credential cannot be written back")

// errCredentialSavedAgain: the credential was saved again after the save being undone (a newer save is never
// overwritten).
var errCredentialSavedAgain = errors.New("the credential was saved again meanwhile")

// snapshotCredential reads resource connID's stored credential for a later restoreCredential; a keystore
// failure other than "none"/"cannot be decrypted" is returned — a save that could not be undone must not start.
func (s *AzureModelService) snapshotCredential(owner storageif.AzureOwner, scope, connID string) (azureCredentialSnapshot, error) {
	key := credentialKey(connID)
	if owner.IsOrg() {
		if s.orgKeys == nil {
			return azureCredentialSnapshot{}, ErrOrgKeyStoreUnavailable
		}
		snap, found, err := s.orgKeys.SnapshotOrg(owner.ID, key)
		if err != nil {
			return azureCredentialSnapshot{}, fmt.Errorf("read the stored credential of Azure resource %q: %w", connID, err)
		}
		return azureCredentialSnapshot{exists: found, restorable: true, org: snap}, nil
	}
	raw, err := s.secrets.Get(scope, key)
	switch {
	case errors.Is(err, storage.ErrSecretNotFound):
		return azureCredentialSnapshot{restorable: true}, nil
	case errors.Is(err, storage.ErrSecretDecryptFailed):
		return azureCredentialSnapshot{exists: true}, nil
	case err != nil:
		return azureCredentialSnapshot{}, fmt.Errorf("read the stored credential of Azure resource %q: %w", connID, err)
	case raw == "":
		return azureCredentialSnapshot{restorable: true}, nil
	}
	return azureCredentialSnapshot{exists: true, restorable: true, raw: raw}, nil
}

// stamp is the org row's CAS token the snapshot saw (zero for no row).
func (c azureCredentialSnapshot) stamp() time.Time {
	if !c.exists {
		return time.Time{}
	}
	return c.org.UpdatedAt
}

// restoreCredential puts snap back after a save: the previous document (an org's with its writer, lender and
// stamp, only while the row still carries savedAt) — or no credential when there was none.
func (s *AzureModelService) restoreCredential(owner storageif.AzureOwner, scope, connID string, snap azureCredentialSnapshot, savedAt time.Time) error {
	if !snap.restorable {
		return errCredentialNotRestorable
	}
	key := credentialKey(connID)
	if owner.IsOrg() {
		var prev *storageif.OrgKeySnapshot
		if snap.exists {
			p := snap.org
			prev = &p
		}
		restored, err := s.orgKeys.RestoreOrgIfUnchanged(owner.ID, key, savedAt, prev)
		if err != nil {
			return err
		}
		if !restored {
			return errCredentialSavedAgain
		}
		return nil
	}
	if !snap.exists {
		if err := s.secrets.Delete(scope, key); err != nil && !errors.Is(err, storage.ErrSecretNotFound) {
			return err
		}
		return nil
	}
	return s.secrets.Save(scope, key, snap.raw)
}

// undoCredentialSave answers cause, a failure after connID's credential was saved: the previous credential goes
// back and cause is returned (forgetSaved releases the briefly stored one), or an AzureCredentialChangedError
// around cause when it cannot go back, so the failed request's change is audited.
func (s *AzureModelService) undoCredentialSave(owner storageif.AzureOwner, scope, connID string, snap azureCredentialSnapshot, savedAt time.Time, forgetSaved func(), cause error) error {
	if err := s.restoreCredential(owner, scope, connID, snap, savedAt); err != nil {
		logger.Error("azure registry: a credential change whose registry write failed could not be undone; the new credential stays in effect",
			"owner", owner.String(), "resource", connID, "cause", cause, "error", err)
		return &AzureCredentialChangedError{Err: cause, ConnectionID: connID}
	}
	if forgetSaved != nil {
		forgetSaved()
	}
	return cause
}

// AzureCredentialChangedError is the failure of a write that replaced a stored credential and then failed,
// when the previous credential could not be put back: the new one (for an organisation, with its new lender)
// is in effect although the request failed. Nothing but the audit log records who put it there, so the handler
// writes the audit row before answering Err.
type AzureCredentialChangedError struct {
	Err error
	// ConnectionID is the resource whose credential changed ("default" for the single-document routes).
	ConnectionID string
}

func (e *AzureCredentialChangedError) Error() string { return e.Err.Error() }

func (e *AzureCredentialChangedError) Unwrap() error { return e.Err }

// AzureCredentialChanged reports whether err is a failed write that left a new credential in effect, and whose.
func AzureCredentialChanged(err error) (connectionID string, changed bool) {
	var ce *AzureCredentialChangedError
	if errors.As(err, &ce) {
		return ce.ConnectionID, true
	}
	return "", false
}
