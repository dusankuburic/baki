package service

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/storage"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-core/logger"
)

// Credential lifecycle across the Azure key family. A user's (or an
// organisation's) Azure credentials are a key FAMILY — "azure-foundry" (the
// default resource) and "azure-foundry/<id>" (every other one) — and
// everything that used to release the one document releases the whole
// family: a lender's disable, an organisation's deletion, an account's
// erasure. What the process keeps for a credential (its Entra ID token
// source, its cached listings) goes with it (ai.ForgetAzureFoundryCredential,
// once per key). Registry rows stay where a credential is disabled — their
// resources read needs_credentials — and cascade where their owner goes.
//
// Two stores, no transaction between them, leave one kind of debris: a
// credential without a registry row (a create whose row write failed after
// the keystore save, a commit that failed after the answer). The orphan
// sweep removes it, at most hourly per owner and only past a grace period
// that covers every create still in flight. The desktop keyring cannot be
// listed; its two-step create rolls back instead (rollbackStalePending).

// azureKeyFamily is the Azure resources' KeyStore key family.
const azureKeyFamily = ai.AzureFoundryProviderID

// Sweep and rollback tuning.
const (
	// azureSweepInterval rate-limits the orphan sweep per owner.
	azureSweepInterval = time.Hour
	// azureOrphanGrace protects a credential younger than this from the
	// sweep: an in-flight create saved it and has not written its row yet.
	azureOrphanGrace = time.Hour
	// azurePendingGrace protects a pending row younger than this from the
	// desktop rollback: another process sharing the directory (a standalone
	// backend next to the app) may still be finishing it.
	azurePendingGrace = 15 * time.Minute
)

// forgetAzureDocument releases what the process keeps for a stored
// document (nothing for one that does not parse).
func forgetAzureDocument(raw string) {
	if raw == "" {
		return
	}
	if cfg, err := ai.ParseAzureFoundryConfig(raw); err == nil {
		ai.ForgetAzureFoundryCredential(cfg)
	}
}

// ---- Lender disable ----

// disableOrgAzureFamily deletes, in one statement, every Azure credential of
// orgID whose secret userID lent (secret_set_by; updated_by for a row no
// save has stamped since v24) and releases each one's process state.
// Returns the keys removed. A store without key families (none in
// production) removes the default resource's key alone, as before.
func disableOrgAzureFamily(orgKeys OrgKeyStore, orgID, userID string) ([]string, error) {
	if fam, ok := orgKeys.(storageif.OrgSecretFamilyStore); ok {
		deleted, err := fam.DeleteOrgKeyFamilyIfSavedBy(orgID, azureKeyFamily, userID)
		if err != nil {
			return nil, err
		}
		keys := make([]string, 0, len(deleted))
		for _, d := range deleted {
			keys = append(keys, d.Provider)
			if !d.Unreadable {
				forgetAzureDocument(d.Document)
			}
		}
		return keys, nil
	}
	prev, hadPrev := storedAzureConfig(func() (string, error) { return orgKeys.GetOrg(orgID, azureKeyFamily) })
	deleted, err := orgKeys.DeleteOrgIfSavedBy(orgID, azureKeyFamily, userID)
	if err != nil || !deleted {
		return nil, err
	}
	if hadPrev {
		ai.ForgetAzureFoundryCredential(prev)
	}
	return []string{azureKeyFamily}, nil
}

// orgsWithAzureFamilySavedBy lists the (org, key) pairs of the Azure family
// userID lent, ordered by org then key.
func orgsWithAzureFamilySavedBy(orgKeys OrgKeyStore, userID string) ([]storageif.OrgKeyRef, error) {
	if fam, ok := orgKeys.(storageif.OrgSecretFamilyStore); ok {
		return fam.OrgKeyFamilySavedBy(azureKeyFamily, userID)
	}
	orgIDs, err := orgKeys.OrgsWithKeySavedBy(azureKeyFamily, userID)
	if err != nil {
		return nil, err
	}
	out := make([]storageif.OrgKeyRef, 0, len(orgIDs))
	for _, id := range orgIDs {
		out = append(out, storageif.OrgKeyRef{OrgID: id, Provider: azureKeyFamily})
	}
	return out, nil
}

// ---- Organisation deletion and account erasure ----

// orgAzureDocuments reads every Azure credential of orgID (while the rows
// still exist) for their release: the family, or the default resource's
// key alone on a store without families.
func orgAzureDocuments(orgKeys OrgKeyStore, orgID string) []string {
	keys := []string{azureKeyFamily}
	if fam, ok := orgKeys.(storageif.OrgSecretFamilyStore); ok {
		entries, err := fam.ListOrgKeyFamily(orgID, azureKeyFamily)
		if err != nil {
			logger.Warn("azure ai foundry: listing an organisation's credentials for release failed", "org", orgID, "error", err)
		} else {
			keys = keys[:0]
			for _, e := range entries {
				keys = append(keys, e.Provider)
			}
		}
	}
	var docs []string
	for _, k := range keys {
		if raw, err := orgKeys.GetOrg(orgID, k); err == nil && raw != "" {
			docs = append(docs, raw)
		}
	}
	return docs
}

// userAzureDocuments reads every Azure credential of the user with key
// scope scope (while they still exist): the family when the keystore lists
// it, else the default resource's key plus the keys of the registry's
// resources.
func userAzureDocuments(secrets KeyStore, scope string, registryIDs []string) []string {
	keys := []string{azureKeyFamily}
	if lister, ok := secrets.(storageif.SecretFamilyLister); ok {
		if entries, err := lister.ListFamily(scope, azureKeyFamily); err == nil {
			keys = keys[:0]
			for _, e := range entries {
				keys = append(keys, e.Provider)
			}
		} else {
			logger.Warn("azure ai foundry: listing the user's credentials for release failed", "error", err)
		}
	} else {
		for _, id := range registryIDs {
			if id != ai.AzureDefaultConnectionID {
				keys = append(keys, ai.AzureConnectionKey(id))
			}
		}
	}
	var docs []string
	for _, k := range keys {
		if raw, err := secrets.Get(scope, k); err == nil && raw != "" {
			docs = append(docs, raw)
		}
	}
	return docs
}

// registryConnectionIDs lists owner's resource ids (best effort: none on a
// failure — the release is best effort too).
func (s *AzureModelService) registryConnectionIDs(ctx context.Context, owner storageif.AzureOwner) []string {
	if s == nil {
		return nil
	}
	conns, err := s.registry.ListConnections(ctx, owner)
	if err != nil {
		return nil
	}
	ids := make([]string, 0, len(conns))
	for _, c := range conns {
		ids = append(ids, c.ID)
	}
	return ids
}

// ---- Orphan sweep ----

// sweepDue reports (and records) that owner's sweep is due.
func (s *AzureModelService) sweepDue(owner storageif.AzureOwner) bool {
	s.memoMu.Lock()
	defer s.memoMu.Unlock()
	now := s.now()
	if at, ok := s.sweptMemo[owner]; ok && now.Sub(at) < azureSweepInterval {
		return false
	}
	if len(s.sweptMemo) >= azureMemoPrune {
		for o, at := range s.sweptMemo {
			if now.Sub(at) >= azureSweepInterval {
				delete(s.sweptMemo, o)
			}
		}
	}
	s.sweptMemo[owner] = now
	return true
}

// sweepOrphans removes owner's Azure credentials that have no registry row
// and were last saved more than azureOrphanGrace ago (at most hourly per
// owner, on the owner's listing). Only once the legacy import ran — until
// then the legacy document has no row by design — and only keys naming a
// generated resource id: the bare legacy key ("azure-foundry", resource
// "default") is never swept — it is the credential a binary rolled back to
// the single-document release runs on, and only an explicit delete removes
// it. Each delete is conditional on the stamp the listing saw, so a key
// saved again since is kept. The keyring, which cannot be listed, is never
// swept.
func (s *AzureModelService) sweepOrphans(ctx context.Context, st *azureOwnerState) {
	owner := st.owner
	var list func() ([]storageif.SecretFamilyEntry, error)
	var del func(key string, at time.Time) (bool, error)
	var get func(key string) (string, error)
	if owner.IsOrg() {
		fam, ok := s.orgKeys.(storageif.OrgSecretFamilyStore)
		if !ok {
			return
		}
		list = func() ([]storageif.SecretFamilyEntry, error) {
			entries, err := fam.ListOrgKeyFamily(owner.ID, azureKeyFamily)
			out := make([]storageif.SecretFamilyEntry, 0, len(entries))
			for _, e := range entries {
				out = append(out, storageif.SecretFamilyEntry{Provider: e.Provider, UpdatedAt: e.UpdatedAt})
			}
			return out, err
		}
		del = func(key string, at time.Time) (bool, error) { return fam.DeleteOrgKeyIfUnchanged(owner.ID, key, at) }
		get = func(key string) (string, error) { return s.orgKeys.GetOrg(owner.ID, key) }
	} else {
		lister, ok := s.secrets.(storageif.SecretFamilyLister)
		if !ok {
			return
		}
		list = func() ([]storageif.SecretFamilyEntry, error) { return lister.ListFamily(st.scope, azureKeyFamily) }
		del = func(key string, at time.Time) (bool, error) { return lister.DeleteIfUnchanged(st.scope, key, at) }
		get = func(key string) (string, error) { return s.secrets.Get(st.scope, key) }
	}
	if !s.sweepDue(owner) {
		return
	}
	if _, imported, err := s.registry.ImportedAt(ctx, owner); err != nil || !imported {
		return
	}
	rows, err := s.registry.ListConnections(ctx, owner) // pending rows included: theirs are not orphans
	if err != nil {
		return
	}
	has := map[string]bool{}
	for _, c := range rows {
		has[c.ID] = true
	}
	entries, err := list()
	if err != nil {
		logger.Warn("azure registry: listing credentials for the orphan sweep failed", "owner", owner.String(), "error", err)
		return
	}
	cutoff := s.now().Add(-azureOrphanGrace)
	for _, e := range entries {
		if e.Provider == azureKeyFamily {
			continue // the legacy key: only an explicit delete removes it
		}
		connID, ok := ai.ParseAzureConnectionKey(e.Provider)
		if !ok || has[connID] || e.UpdatedAt.IsZero() || e.UpdatedAt.After(cutoff) {
			continue
		}
		raw, _ := get(e.Provider)
		deleted, err := del(e.Provider, e.UpdatedAt)
		if err != nil {
			logger.Warn("azure registry: deleting an orphaned credential failed", "owner", owner.String(), "key", e.Provider, "error", err)
			continue
		}
		if !deleted {
			continue // saved again since the listing: no orphan any more
		}
		forgetAzureDocument(raw)
		logger.Info("azure registry: deleted an orphaned credential", "owner", owner.String(), "key", e.Provider)
	}
}

// credentialStamp reads the stamp of resource connID's stored credential —
// the time its row was last saved — for a delete decided now and run later
// (deleteCredentialIfUnchanged): known is false when the keystore cannot
// delete conditionally (the desktop keyring) or the read failed; a zero
// stamp with known true means there is no credential.
func (s *AzureModelService) credentialStamp(owner storageif.AzureOwner, scope, connID string) (stamp time.Time, known bool) {
	key := credentialKey(connID)
	if owner.IsOrg() {
		if _, ok := s.orgKeys.(storageif.OrgSecretFamilyStore); !ok {
			return time.Time{}, false
		}
		meta, found, err := s.orgKeys.OrgKeyMeta(owner.ID, key)
		if err != nil {
			return time.Time{}, false
		}
		if !found {
			return time.Time{}, true
		}
		return meta.UpdatedAt, true
	}
	lister, ok := s.secrets.(storageif.SecretFamilyLister)
	if !ok {
		return time.Time{}, false
	}
	entries, err := lister.ListFamily(scope, azureKeyFamily)
	if err != nil {
		return time.Time{}, false
	}
	for _, e := range entries {
		if e.Provider == key {
			return e.UpdatedAt, true
		}
	}
	return time.Time{}, true
}

// deleteCredentialIfUnchanged deletes resource connID's credential as
// credentialStamp saw it: only while its row still carries stamp — one saved
// again since (a single-document save of resource "default", a rotation
// racing the delete) is kept — nothing when there was none, and
// unconditionally when the stamp is not known (the keyring: one local user,
// no racing writer). deleted reports whether a credential went.
func (s *AzureModelService) deleteCredentialIfUnchanged(owner storageif.AzureOwner, scope, connID string, stamp time.Time, known bool) (deleted bool, err error) {
	if !known {
		return true, s.deleteCredential(owner, scope, connID)
	}
	if stamp.IsZero() {
		return false, nil
	}
	key := credentialKey(connID)
	if owner.IsOrg() {
		if fam, ok := s.orgKeys.(storageif.OrgSecretFamilyStore); ok {
			return fam.DeleteOrgKeyIfUnchanged(owner.ID, key, stamp)
		}
	} else if lister, ok := s.secrets.(storageif.SecretFamilyLister); ok {
		return lister.DeleteIfUnchanged(scope, key, stamp)
	}
	return true, s.deleteCredential(owner, scope, connID)
}

// ---- Desktop pending rows ----

// rollbackStalePending rolls back the pending rows an earlier process left
// (a create that never finished): its keyring entry, when the save got that
// far, then the row. A row stays while it is this process's (a create in
// flight) or younger than azurePendingGrace (another process sharing the
// directory may be finishing it) — and every row stays while the registry is
// read-only (a newer app version wrote the file): the row could not be
// removed, and deleting its keyring entry first would leave a row the newer
// app may still finish without its secret.
func (s *AzureModelService) rollbackStalePending(ctx context.Context, owner storageif.AzureOwner, scope string) {
	if !s.registry.PendingWrites() {
		return
	}
	conns, err := s.registry.ListConnections(ctx, owner)
	if err != nil {
		return
	}
	boot := s.registry.BootID()
	cutoff := s.now().Add(-azurePendingGrace)
	stale := false
	for _, c := range conns {
		stale = stale || (c.Pending && c.BootID != boot && !c.CreatedAt.After(cutoff))
	}
	if !stale || s.registry.State(ctx).State == storageif.AzureRegistryReadOnly {
		return
	}
	for _, c := range conns {
		if !c.Pending || c.BootID == boot || c.CreatedAt.After(cutoff) {
			continue
		}
		if err := s.deleteCredential(owner, scope, c.ID); err != nil {
			logger.Warn("azure registry: rolling back a pending resource: its keyring entry could not be deleted", "resource", c.ID, "error", err)
			continue
		}
		if _, err := s.registry.DeleteConnection(ctx, owner, c.ID); err != nil && !errors.Is(err, storageif.ErrAzureConnectionNotFound) {
			logger.Warn("azure registry: rolling back a pending resource failed", "resource", c.ID, "error", err)
			continue
		}
		logger.Info("azure registry: rolled back a resource an earlier run left unfinished", "resource", c.ID)
	}
}

// ---- GDPR export ----

// ExportConnectionConfigs is the data-subject export's source of userID's
// resources' public configuration (storageif.AzureExportConfigFunc): the
// endpoint, the auth mode, the tenant and client ids and the project
// endpoint of each credential — never a secret. A credential that cannot be
// decrypted or parsed is marked Unreadable; a missing one is left out; any
// other keystore failure fails the export (it is complete or nothing).
func (s *AzureModelService) ExportConnectionConfigs(_ context.Context, userID string, connectionIDs []string) (map[string]storageif.AzureConnectionPublicConfig, error) {
	out := make(map[string]storageif.AzureConnectionPublicConfig, len(connectionIDs))
	for _, id := range connectionIDs {
		if !ai.ValidAzureConnectionID(id) {
			continue
		}
		raw, err := s.secrets.Get(userID, credentialKey(id))
		switch {
		case errors.Is(err, storage.ErrSecretNotFound):
			continue
		case errors.Is(err, storage.ErrSecretDecryptFailed):
			out[id] = storageif.AzureConnectionPublicConfig{Unreadable: true}
			continue
		case err != nil:
			return nil, fmt.Errorf("read the credential of Azure resource %q: %w", id, err)
		case raw == "":
			continue
		}
		cfg, perr := ai.ParseAzureFoundryConfig(raw)
		if perr != nil {
			out[id] = storageif.AzureConnectionPublicConfig{Unreadable: true}
			continue
		}
		out[id] = storageif.AzureConnectionPublicConfig{
			Endpoint: cfg.Endpoint, AuthMode: cfg.Auth.Mode, TenantID: cfg.Auth.TenantID,
			ClientID: cfg.Auth.ClientID, ProjectEndpoint: cfg.ProjectEndpoint,
		}
	}
	return out, nil
}

// ---- ProviderService lifecycle (family-wide) ----

// DisableOrgAzureCredentialsSavedBy disables every Azure AI Foundry
// credential of orgID that userID lent — called when that admin is demoted,
// removed from the organisation or deletes their account: every key of the
// family whose secret they supplied is deleted in one statement (a newer
// save by another admin with their own secret is kept, an edit that carried
// the secret does not make its editor a lender), each credential's process
// state is released and the organisation's cached listings dropped. The
// registry rows stay: their resources read needs_credentials until an admin
// enters the credentials again. Returns the keys disabled — one
// keys.save {op:"disable"} audit row each; none without an org store.
func (s *ProviderService) DisableOrgAzureCredentialsSavedBy(orgID, userID string) (keys []string, err error) {
	defer logger.Guard("App.DisableOrgAzureCredentialsSavedBy", &err)
	if s.orgKeys == nil || orgID == "" || userID == "" {
		return nil, nil
	}
	keys, err = disableOrgAzureFamily(s.orgKeys, orgID, userID)
	if err != nil {
		return nil, err
	}
	if len(keys) > 0 {
		s.InvalidateScope(ai.OrgScope(orgID))
	}
	return keys, nil
}

// DisableOrgAzureCredentialsLentBy is DisableOrgAzureCredentialsSavedBy for
// every organisation userID lent an Azure credential to (account erasure:
// the membership rows go with the account, the credential rows do not).
// Returns the (org, key) pairs disabled; an organisation that fails is
// logged and skipped so the others are still disabled.
func (s *ProviderService) DisableOrgAzureCredentialsLentBy(userID string) (refs []storageif.OrgKeyRef, err error) {
	defer logger.Guard("App.DisableOrgAzureCredentialsLentBy", &err)
	if s.orgKeys == nil || userID == "" {
		return nil, nil
	}
	candidates, err := orgsWithAzureFamilySavedBy(s.orgKeys, userID)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var orgIDs []string
	for _, c := range candidates {
		if !seen[c.OrgID] {
			seen[c.OrgID] = true
			orgIDs = append(orgIDs, c.OrgID)
		}
	}
	sort.Strings(orgIDs)
	for _, orgID := range orgIDs {
		keys, derr := s.DisableOrgAzureCredentialsSavedBy(orgID, userID)
		if derr != nil {
			logger.Warn("azure ai foundry: disabling a departed admin's shared credentials failed", "org", orgID, "error", derr)
			continue
		}
		for _, k := range keys {
			refs = append(refs, storageif.OrgKeyRef{OrgID: orgID, Provider: k})
		}
	}
	return refs, nil
}
