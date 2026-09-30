package service

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/storage"
	storagedb "pad-analyzer/internal/storage/database"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-core/logger"
)

// ---- The Azure AI Foundry model registry (Part 9) ----
//
// A user — and, in cloud mode, an organisation — keeps up to ten Azure AI
// Foundry RESOURCES ("connections" in code and API, "resources" in the UI:
// Foundry has a "Connections" concept of its own), each with its own endpoint
// and credential document, and up to fifty registered MODELS across them:
// chat and Claude deployments, project agents and embedding deployments, each
// with a display name, a purpose and a description. The registry metadata
// lives in the AzureRegistryStore (Postgres with auth on, else the desktop's
// azure-models.json); every resource's credential stays in the KeyStore — the
// default resource's (the legacy single document) under "azure-foundry",
// every other one's under "azure-foundry/<id>" (ai.AzureConnectionKey).
//
// Two stores, no transaction spanning them, so the write order is fixed
// (see createConnection / deleteConnection): a create or rotation validates,
// pre-checks the limit, saves the keystore, THEN takes the owner lock,
// re-checks and writes the registry — an orphaned secret (swept later) is
// possible, a registry row without its secret is not; a delete removes the
// row in the (request) transaction and the secret only after the commit
// (storagedb.RegisterPostCommit). Nothing outbound — and no keystore call —
// runs while the owner lock is held. The desktop has neither transaction nor
// a listable keyring, so a resource is created pending, its keyring entry
// saved, then finalised; a pending row an earlier process left behind is
// rolled back on the next listing.
//
// Status is computed when read, never stored: a resource is ready,
// needs_credentials, unreadable or invalid_config; a model inherits its
// resource's status or is unsupported (an agent on a resource without
// Entra ID and a project endpoint).

// Registry status values (views, the picker's ModelDetail.Status).
const (
	AzureStatusReady            = "ready"
	AzureStatusNeedsCredentials = "needs_credentials" // #nosec G101 -- a status value, not a credential
	AzureStatusUnreadable       = "unreadable"
	AzureStatusInvalidConfig    = "invalid_config"
	AzureStatusUnsupported      = "unsupported"
)

// Registry client codes (Part 9 "Error codes"), next to the credential
// document's *ai.AzureConfigError codes (INVALID_ENDPOINT, HOST_NOT_ALLOWED,
// INVALID_AUTH, MISSING_SECRET, INVALID_DEPLOYMENT, …) and the registry
// validation codes ai owns (ai.AzureCfgErrInvalidKind,
// ai.AzureCfgErrInvalidContextWindow, ai.AzureCfgErrInvalidMaxOutput).
const (
	AzureCodeConnectionNotFound       = "CONNECTION_NOT_FOUND"
	AzureCodeModelNotFound            = "MODEL_NOT_FOUND"
	AzureCodeConnectionInUse          = "CONNECTION_IN_USE"
	AzureCodeDuplicateModel           = "DUPLICATE_MODEL"
	AzureCodeDuplicateConnectionName  = "DUPLICATE_CONNECTION_NAME"
	AzureCodeTooManyConnections       = "TOO_MANY_CONNECTIONS"
	AzureCodeTooManyModels            = "TOO_MANY_MODELS"
	AzureCodeConflict                 = "CONFLICT"
	AzureCodeReindexRequired          = "REINDEX_REQUIRED"
	AzureCodeInvalidPurpose           = "INVALID_PURPOSE"
	AzureCodeInvalidDescription       = "INVALID_DESCRIPTION"
	AzureCodeInvalidDisplayName       = "INVALID_DISPLAY_NAME"
	AzureCodeInvalidConnectionName    = "INVALID_CONNECTION_NAME"
	AzureCodeInvalidDesignation       = "INVALID_DESIGNATION"
	AzureCodeKindRequiresProject      = "KIND_REQUIRES_PROJECT"
	AzureCodeDiscoveryUnavailable     = "DISCOVERY_UNAVAILABLE"
	AzureCodeModelNotAvailable        = "MODEL_NOT_AVAILABLE"
	AzureCodeSecretStorageUnavailable = "SECRET_STORAGE_UNAVAILABLE"
	// AzureCodeRegistryReadOnly: the desktop list file was written by a
	// newer app version (registryState read_only) — writes are refused.
	AzureCodeRegistryReadOnly        = "REGISTRY_READ_ONLY"
	AzureCodeOrgKeystoreUnavailable  = "ORG_KEYSTORE_UNAVAILABLE"
	AzureCodeOrgCredentialUnreadable = "ORG_CREDENTIAL_UNREADABLE" // #nosec G101 -- an error code, not a credential
)

// ErrModelNotAvailable is ResolveChat's answer for a model id the caller
// cannot use: an unknown or malformed id, another owner's entry, an
// embedding model, an entry of an organisation whose shared models the caller
// no longer may use, or a raw (pre-registry) id no alias or legacy document
// resolves. One answer for all of them, so the id space is no existence
// oracle. The chat handlers map it to 400 MODEL_NOT_AVAILABLE; a registry or
// keystore failure is never this error (and never a fallback).
var ErrModelNotAvailable = errors.New("the selected Azure AI Foundry model is not available — it was removed, or you can no longer use it; choose another model")

// AzureRegistryError is a refusal of the registry with its client code: the
// HTTP status it maps to, the code, one line of copy and optional details
// (limit, modelCount, existingModelId, field). Err is the cause (a storage
// sentinel, an *ai.AzureConfigError), matched through errors.Is / As.
type AzureRegistryError struct {
	Status  int
	Code    string
	Message string
	Details map[string]any
	Err     error
}

func (e *AzureRegistryError) Error() string { return e.Message }

func (e *AzureRegistryError) Unwrap() error { return e.Err }

func azureRegErr(status int, code, message string, cause error, details map[string]any) error {
	return &AzureRegistryError{Status: status, Code: code, Message: message, Err: cause, Details: details}
}

// AzureRegistryHTTPError maps an error of the registry's methods (and of the
// single-document routes that act on resource "default") to its HTTP answer:
// the status, the client code and the details of the error envelope. ok is
// false for anything else — an internal failure the handler renders as 500
// with generic copy. Every code of Part 9's table is covered:
//
//	404 CONNECTION_NOT_FOUND, MODEL_NOT_FOUND, ORG_KEYSTORE_UNAVAILABLE
//	409 CONNECTION_IN_USE {modelCount}, DUPLICATE_MODEL {existingModelId},
//	    DUPLICATE_CONNECTION_NAME, TOO_MANY_CONNECTIONS {limit},
//	    TOO_MANY_MODELS {limit}, CONFLICT, REINDEX_REQUIRED, REGISTRY_READ_ONLY
//	400 every *ai.AzureConfigError code (INVALID_AUTH and MISSING_SECRET with
//	    {field}), INVALID_KIND, INVALID_PURPOSE, INVALID_DESCRIPTION,
//	    INVALID_DISPLAY_NAME, INVALID_CONTEXT_WINDOW, INVALID_MAX_OUTPUT,
//	    INVALID_CONNECTION_NAME, INVALID_DESIGNATION, KIND_REQUIRES_PROJECT,
//	    DISCOVERY_UNAVAILABLE, MODEL_NOT_AVAILABLE
//	500 ORG_CREDENTIAL_UNREADABLE
//	503 SECRET_STORAGE_UNAVAILABLE
func AzureRegistryHTTPError(err error) (status int, code string, details map[string]any, ok bool) {
	if err == nil {
		return 0, "", nil, false
	}
	var re *AzureRegistryError
	if errors.As(err, &re) {
		return re.Status, re.Code, re.Details, true
	}
	var ce *ai.AzureConfigError
	if errors.As(err, &ce) {
		return http.StatusBadRequest, ce.Code, nil, true
	}
	switch {
	case errors.Is(err, ErrModelNotAvailable):
		return http.StatusBadRequest, AzureCodeModelNotAvailable, nil, true
	case errors.Is(err, storageif.ErrAzureConnectionNotFound):
		return http.StatusNotFound, AzureCodeConnectionNotFound, nil, true
	case errors.Is(err, storageif.ErrAzureModelNotFound):
		return http.StatusNotFound, AzureCodeModelNotFound, nil, true
	case errors.Is(err, storageif.ErrAzureDuplicateConnectionName):
		return http.StatusConflict, AzureCodeDuplicateConnectionName, nil, true
	case errors.Is(err, storageif.ErrAzureDuplicateModel):
		return http.StatusConflict, AzureCodeDuplicateModel, nil, true
	case errors.Is(err, storageif.ErrAzureTooManyConnections):
		return http.StatusConflict, AzureCodeTooManyConnections, map[string]any{"limit": storageif.AzureMaxConnections}, true
	case errors.Is(err, storageif.ErrAzureTooManyModels):
		return http.StatusConflict, AzureCodeTooManyModels, map[string]any{"limit": storageif.AzureMaxModels}, true
	case errors.Is(err, storageif.ErrAzureRegistryConflict), errors.Is(err, ErrOrgCredentialConflict),
		errors.Is(err, storageif.ErrAzureDuplicateDesignation):
		return http.StatusConflict, AzureCodeConflict, nil, true
	case errors.Is(err, storageif.ErrAzureInvalidDesignation):
		return http.StatusBadRequest, AzureCodeInvalidDesignation, nil, true
	case errors.Is(err, storageif.ErrAzureRegistryReadOnly):
		return http.StatusConflict, AzureCodeRegistryReadOnly, nil, true
	case errors.Is(err, ai.ErrAzureDiscoveryUnavailable):
		return http.StatusBadRequest, AzureCodeDiscoveryUnavailable, nil, true
	case errors.Is(err, storage.ErrSecretStorageUnavailable):
		return http.StatusServiceUnavailable, AzureCodeSecretStorageUnavailable, nil, true
	case errors.Is(err, ErrOrgKeyStoreUnavailable):
		return http.StatusNotFound, AzureCodeOrgKeystoreUnavailable, nil, true
	case errors.Is(err, ErrOrgCredentialUnreadable):
		return http.StatusInternalServerError, AzureCodeOrgCredentialUnreadable, nil, true
	}
	return 0, "", nil, false
}

// ---- Dependencies ----

// AzureOrgDirectory is the part of the organisation store the registry
// reads: which organisations a user belongs to, with each member's role (the
// "Shared with you" section, the knowledge base's corpus check), and one
// organisation's name.
type AzureOrgDirectory interface {
	ListOrgsForUser(ctx context.Context, userID string) ([]*storageif.Organisation, error)
	LoadOrg(ctx context.Context, id string) (*storageif.Organisation, error)
}

// AzureCorpusCounter counts an organisation's knowledge-base chunks: a
// change to the corpus's embedding model on a non-empty corpus needs a
// re-index (REINDEX_REQUIRED).
type AzureCorpusCounter interface {
	CountKnowledgeChunks(ctx context.Context, orgID string) (total, vectorIndexed int, err error)
}

// AzureUserDirectory names users (a shared resource's lender).
type AzureUserDirectory interface {
	LoadUserByID(ctx context.Context, id string) (*storageif.User, error)
}

// AzureSettingsLoader reads a user's own settings (cloud): the legacy import
// reads the saved "Default for new chats" from them.
type AzureSettingsLoader interface {
	LoadUserSettings(ctx context.Context, userID string) (*storageif.AppSettings, error)
}

// AzureModelServiceDeps is what NewAzureModelService needs. Registry and
// Secrets are required; the rest are optional (nil disables what needs them).
type AzureModelServiceDeps struct {
	Registry storageif.AzureRegistryStore
	Secrets  KeyStore
	// OrgKeys holds organisations' shared credentials (cloud); nil disables
	// organisation registries (ErrOrgKeyStoreUnavailable).
	OrgKeys OrgKeyStore
	// Factory builds the providers the picker sizes its models with and the
	// knowledge base embeds with; nil = no provider-derived windows.
	Factory *ai.ProviderFactory
	// Settings is the desktop settings store: the legacy import reads the
	// saved default model from it when the registry is the desktop's.
	Settings SettingsProvider
	// UserSettings reads a user's own settings (cloud; usually the storage
	// backend). Used with PerUserOwners.
	UserSettings AzureSettingsLoader
	Orgs         AzureOrgDirectory
	Corpus       AzureCorpusCounter
	Users        AzureUserDirectory
	// PerUserOwners: the registry keeps one registry per user and one per
	// organisation (Postgres with auth on). False for the desktop's single
	// personal registry (owner ""), which also rules out organisations.
	PerUserOwners bool
	// KnowledgeEmbeddingEnabled: the knowledge base embeds with Azure AI
	// Foundry here (cloud with PAD_EMBEDDING_PROVIDER=azure-foundry), so the
	// "Use for knowledge base" designation is offered.
	KnowledgeEmbeddingEnabled bool
}

// AzureModelService is the Azure AI Foundry model registry: resources and
// models (CRUD, designations, validation, status), the legacy import and the
// single-document adapters, the chat runtime's resolution and picker
// listing, the knowledge base's embedder, tests and discovery, and the
// credential lifecycle (see the file comment).
type AzureModelService struct {
	registry                  storageif.AzureRegistryStore
	secrets                   KeyStore
	orgKeys                   OrgKeyStore
	factory                   *ai.ProviderFactory
	settings                  SettingsProvider
	userSettings              AzureSettingsLoader
	orgs                      AzureOrgDirectory
	corpus                    AzureCorpusCounter
	users                     AzureUserDirectory
	perUserOwners             bool
	knowledgeEmbeddingEnabled bool

	// now is the clock (tests move it).
	now func() time.Time

	memoMu sync.Mutex
	// importedMemo remembers owners whose import marker was seen (60 s,
	// recorded only after the commit that made it durable).
	importedMemo map[storageif.AzureOwner]time.Time
	// sweptMemo rate-limits the orphan sweep (hourly per owner).
	sweptMemo map[storageif.AzureOwner]time.Time
	// deferredMemo remembers owners whose legacy import is waiting for a
	// free resource slot or name (errLegacyImportDeferred): retried at most
	// once per azureImportMemoTTL — a resource removed clears it, so a slot
	// freed is used at the next read.
	deferredMemo map[storageif.AzureOwner]time.Time

	// refresher refreshes stale project listings in the background.
	refresher *azureListingRefresher

	// Test seams: the network-touching ai entry points.
	probeResource func(ctx context.Context, raw string) error
	probeModel    func(ctx context.Context, raw string, m ai.AzureRegisteredModel) error
	discover      func(ctx context.Context, raw string, registered []ai.AzureRegisteredModel) (ai.AzureDiscovery, error)
}

// NewAzureModelService builds the registry service.
func NewAzureModelService(deps AzureModelServiceDeps) *AzureModelService {
	return &AzureModelService{
		registry:                  deps.Registry,
		secrets:                   deps.Secrets,
		orgKeys:                   deps.OrgKeys,
		factory:                   deps.Factory,
		settings:                  deps.Settings,
		userSettings:              deps.UserSettings,
		orgs:                      deps.Orgs,
		corpus:                    deps.Corpus,
		users:                     deps.Users,
		perUserOwners:             deps.PerUserOwners,
		knowledgeEmbeddingEnabled: deps.KnowledgeEmbeddingEnabled,
		now:                       time.Now,
		importedMemo:              map[storageif.AzureOwner]time.Time{},
		sweptMemo:                 map[storageif.AzureOwner]time.Time{},
		deferredMemo:              map[storageif.AzureOwner]time.Time{},
		refresher:                 newAzureListingRefresher(ai.RefreshAzureFoundryListing),
		probeResource:             ai.ProbeAzureFoundryResource,
		probeModel:                ai.ProbeAzureFoundryModel,
		discover:                  ai.DiscoverAzureFoundryProject,
	}
}

// KnowledgeEmbeddingEnabled reports whether the knowledge base embeds with
// Azure AI Foundry here (the "Use for knowledge base" designation is
// offered).
func (s *AzureModelService) KnowledgeEmbeddingEnabled() bool { return s.knowledgeEmbeddingEnabled }

// OrgRegistriesAvailable reports whether organisations can hold registries
// in this deployment: a per-user registry store (Postgres, auth on) and the
// encrypted org keystore.
func (s *AzureModelService) OrgRegistriesAvailable() bool {
	return s != nil && s.orgKeys != nil && s.perUserOwners
}

// ---- Scopes and owners ----

// AzureScope addresses one registry for one caller.
type AzureScope struct {
	// Scope is the caller's key scope: their user id with authentication on,
	// "" on the desktop. It also names the caller (the acting admin of an
	// organisation's write, the member whose shared entries are listed).
	Scope string
	// OrgID selects the organisation's curated registry; "" is the caller's
	// own. The caller's role in it is the handler's check (reads: admin or
	// member; writes, tests, discovery: admin).
	OrgID string
}

// IsOrg reports whether the scope addresses an organisation's registry.
func (as AzureScope) IsOrg() bool { return as.OrgID != "" }

// userOwner is the registry of the caller with key scope scope: their own
// with per-user registries, else the single personal registry of the
// desktop's file store.
func (s *AzureModelService) userOwner(scope string) storageif.AzureOwner {
	if s.perUserOwners {
		return storageif.AzureUserOwner(scope)
	}
	return storageif.AzureUserOwner("")
}

// ownerFor is the registry as addresses: ErrOrgKeyStoreUnavailable for an
// organisation where organisations hold none.
func (s *AzureModelService) ownerFor(as AzureScope) (storageif.AzureOwner, error) {
	if !as.IsOrg() {
		return s.userOwner(as.Scope), nil
	}
	if !s.OrgRegistriesAvailable() {
		return storageif.AzureOwner{}, ErrOrgKeyStoreUnavailable
	}
	return storageif.AzureOrgOwner(as.OrgID), nil
}

// ---- Credentials ----

// credentialKey is the KeyStore key of resource connID.
func credentialKey(connID string) string { return ai.AzureConnectionKey(connID) }

// readCredential reads the stored document of one resource of owner: "" when
// there is none. scope is the caller's key scope (personal documents live
// under it, whatever the registry's owner id).
func (s *AzureModelService) readCredential(owner storageif.AzureOwner, scope, connID string) (string, error) {
	var (
		raw string
		err error
	)
	if owner.IsOrg() {
		if s.orgKeys == nil {
			return "", ErrOrgKeyStoreUnavailable
		}
		raw, err = s.orgKeys.GetOrg(owner.ID, credentialKey(connID))
	} else {
		raw, err = s.secrets.Get(scope, credentialKey(connID))
	}
	if errors.Is(err, storage.ErrSecretNotFound) {
		return "", nil
	}
	return raw, err
}

// azureCredential is one resource's stored credential as the registry sees
// it: the document (when it parses) and the status it gives the resource.
type azureCredential struct {
	raw    string
	cfg    ai.AzureFoundryConfig
	parsed bool
	status string
	reason string
}

// ready reports a usable document.
func (c azureCredential) ready() bool { return c.status == AzureStatusReady }

// Status copy (statusReason).
const (
	azureReasonNoCredentials    = "No credentials are stored for this resource — enter them again to use its models."
	azureReasonOrgNoCredentials = "The organisation's credentials for this resource were removed (for example, the admin who shared them is no longer an admin) — an org admin must enter them again."
	azureReasonUnreadable       = "The stored credentials could not be read (the server's encryption key may have changed) — enter them again."
	azureReasonOrgCLI           = "The Azure CLI sign-in cannot be shared with an organisation — an org admin must switch this resource to an API key or a Microsoft Entra ID service principal."
	azureReasonAgentUnsupported = "Agents need Microsoft Entra ID (or Azure CLI) sign-in and a project endpoint on their resource."
)

// credentialState reads and classifies resource connID's credential. The
// error is a keystore failure other than "none" and "cannot be decrypted"
// (those are statuses).
func (s *AzureModelService) credentialState(owner storageif.AzureOwner, scope, connID string) (azureCredential, error) {
	raw, err := s.readCredential(owner, scope, connID)
	switch {
	case errors.Is(err, storage.ErrSecretDecryptFailed):
		return azureCredential{status: AzureStatusUnreadable, reason: azureReasonUnreadable}, nil
	case err != nil:
		return azureCredential{}, fmt.Errorf("read the credentials of Azure resource %q: %w", connID, err)
	case strings.TrimSpace(raw) == "":
		reason := azureReasonNoCredentials
		if owner.IsOrg() {
			reason = azureReasonOrgNoCredentials
		}
		return azureCredential{status: AzureStatusNeedsCredentials, reason: reason}, nil
	}
	return classifyCredential(owner.IsOrg(), connID, raw), nil
}

// classifyCredential gives a stored document its status: unreadable when it
// does not parse, needs_credentials without a secret, invalid_config when it
// no longer passes validation (an operator narrowed the allowed hosts, a
// cloud deployment refuses Azure CLI sign-in, an organisation's resource
// signs in with the Azure CLI), else ready.
func classifyCredential(org bool, connID, raw string) azureCredential {
	cfg, err := ai.ParseAzureFoundryConfig(raw)
	if err != nil || !cfg.Configured() {
		return azureCredential{raw: raw, status: AzureStatusUnreadable, reason: azureReasonUnreadable}
	}
	c := azureCredential{raw: raw, cfg: cfg, parsed: true, status: AzureStatusReady}
	if org && cfg.Auth.Mode == ai.AzureAuthModeEntraCLI {
		c.status, c.reason = AzureStatusInvalidConfig, azureReasonOrgCLI
		return c
	}
	if verr := ai.ValidateAzureConnection(cfg, connID != ai.AzureDefaultConnectionID); verr != nil {
		var ce *ai.AzureConfigError
		if errors.As(verr, &ce) && ce.Code == ai.AzureCfgErrMissingSecret {
			c.status, c.reason = AzureStatusNeedsCredentials, azureReasonNoCredentials
			if org {
				c.reason = azureReasonOrgNoCredentials
			}
			return c
		}
		c.status, c.reason = AzureStatusInvalidConfig, "The saved settings are no longer valid: "+verr.Error()
	}
	return c
}

// ---- Views ----

// AzureLastTestView is a saved resource's or model's last test.
type AzureLastTestView struct {
	OK        bool   `json:"ok"`
	LatencyMs int    `json:"latencyMs"`
	Error     string `json:"error,omitempty"`
	At        string `json:"at"`
}

// AzureConnectionConfigView is a resource's public settings: never a secret,
// never deployment fields.
type AzureConnectionConfigView struct {
	Endpoint        string `json:"endpoint"`
	AuthMode        string `json:"authMode"`
	HasSecret       bool   `json:"hasSecret"`
	TenantID        string `json:"tenantId,omitempty"`
	ClientID        string `json:"clientId,omitempty"`
	ProjectEndpoint string `json:"projectEndpoint,omitempty"`
}

// AzureConnectionView is the resource view (GET/POST/PUT /connections).
type AzureConnectionView struct {
	ID string `json:"id"`
	// Name is the resource's display name (1–64 characters).
	Name string `json:"name"`
	// Shared marks an organisation's resource: always in org scope, and on
	// the "Shared with you" entries of user scope (read-only there).
	Shared  bool   `json:"shared"`
	OrgID   string `json:"orgId,omitempty"`
	OrgName string `json:"orgName,omitempty"`
	// Legacy marks the resource the single-document configuration became.
	Legacy       bool                      `json:"legacy"`
	Config       AzureConnectionConfigView `json:"config"`
	Status       string                    `json:"status"`
	StatusReason string                    `json:"statusReason,omitempty"`
	// CanDiscover and SupportsAgents: Entra ID (or the Azure CLI) with a
	// project endpoint — deployments and agents can be discovered, agents run.
	CanDiscover    bool `json:"canDiscover"`
	SupportsAgents bool `json:"supportsAgents"`
	// SuggestDiscovery shows the "Found in your project" prompt: discoverable
	// and not dismissed (never on a read-only shared entry).
	SuggestDiscovery bool               `json:"suggestDiscovery"`
	ModelCount       int                `json:"modelCount"`
	LastTest         *AzureLastTestView `json:"lastTest"`
	// UpdatedAt is the registry row's compare-and-swap token (RFC 3339).
	UpdatedAt string `json:"updatedAt"`
	// CredentialUpdatedAt (org scope) is the stored credential's
	// compare-and-swap token, sent back with a credential change.
	CredentialUpdatedAt string `json:"credentialUpdatedAt,omitempty"`
	// SharedBy names who lent a shared resource's credential (display name,
	// else email); SharedByID is that admin's user id.
	SharedBy   string `json:"sharedBy,omitempty"`
	SharedByID string `json:"-"`
}

// AzureModelView is the model view (GET/POST/PUT /models).
type AzureModelView struct {
	ID             string `json:"id"`
	ModelID        string `json:"modelId"`
	ConnectionID   string `json:"connectionId"`
	ConnectionName string `json:"connectionName"`
	Kind           string `json:"kind"`
	// Deployment is the deployment name, or the agent's name.
	Deployment string `json:"deployment"`
	// Target is what the server routes to: "<deployment>",
	// "claude:<deployment>" or "agent:<name>" (an embedding deployment's
	// name for an embedding model).
	Target        string `json:"target"`
	DisplayName   string `json:"displayName"`
	Purpose       string `json:"purpose"`
	PurposeCustom string `json:"purposeCustom,omitempty"`
	Description   string `json:"description,omitempty"`
	// ContextWindow and MaxOutputTokens are the user's overrides (absent
	// when unset); ReportedContextWindow is the window the server knows from
	// the catalogue, the served model or a learned limit.
	ContextWindow         int                `json:"contextWindow,omitempty"`
	ReportedContextWindow int                `json:"reportedContextWindow,omitempty"`
	MaxOutputTokens       int                `json:"maxOutputTokens,omitempty"`
	IsDefault             bool               `json:"isDefault"`
	IsKnowledgeEmbedding  bool               `json:"isKnowledgeEmbedding"`
	Shared                bool               `json:"shared"`
	OrgID                 string             `json:"orgId,omitempty"`
	OrgName               string             `json:"orgName,omitempty"`
	Status                string             `json:"status"`
	StatusReason          string             `json:"statusReason,omitempty"`
	LastTest              *AzureLastTestView `json:"lastTest"`
	UpdatedAt             string             `json:"updatedAt"`
}

// AzureRegistryLimits are the per-owner limits.
type AzureRegistryLimits struct {
	Connections int `json:"connections"`
	Models      int `json:"models"`
}

// AzureConnectionsListing is GET /connections.
type AzureConnectionsListing struct {
	Connections []AzureConnectionView `json:"connections"`
	// RegistryState is ok, read_only (a newer app wrote the desktop list
	// file) or recovered (a corrupt one was set aside, a copy kept).
	RegistryState string `json:"registryState"`
	// RecoveredCopyPath is where that copy was kept (recovered only). Only
	// the desktop list file is ever recovered, and its API is the local
	// user's own, so the path is theirs to see.
	RecoveredCopyPath         string              `json:"recoveredCopyPath,omitempty"`
	Limits                    AzureRegistryLimits `json:"limits"`
	KnowledgeEmbeddingEnabled bool                `json:"knowledgeEmbeddingEnabled"`
}

// AzureModelsListing is GET /models. ServerDefault (user scope) is the
// server default's "m/<id>" — what a new chat starts on when the user saved
// no default of their own; empty when there is none, and in org scope.
type AzureModelsListing struct {
	Models        []AzureModelView `json:"models"`
	ServerDefault string           `json:"serverDefault,omitempty"`
}

// AzureConnectionEnvelope is the body of POST, PUT and GET
// /connections/{id}: {connection}.
type AzureConnectionEnvelope struct {
	Connection AzureConnectionView `json:"connection"`
}

// AzureModelEnvelope is the body of POST and PUT /models/{id}: {model}.
type AzureModelEnvelope struct {
	Model AzureModelView `json:"model"`
}

// AzureConnectionDeleteResult is DELETE /connections/{id}.
type AzureConnectionDeleteResult struct {
	Status                    string `json:"status"`
	DeletedModels             int    `json:"deletedModels"`
	ClearedKnowledgeEmbedding bool   `json:"clearedKnowledgeEmbedding"`
}

// azureStamp renders a registry timestamp (microseconds, UTC) as the view's
// RFC 3339 string — the compare-and-swap token a PUT sends back.
func azureStamp(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

// parseAzureStamp reads a compare-and-swap token ("" = none).
func parseAzureStamp(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, &ai.AzureConfigError{Code: ai.AzureCfgErrInvalidConfig, Message: "updatedAt must be the RFC 3339 timestamp the resource or model was loaded with"}
	}
	return t, nil
}

func lastTestView(r *storageif.AzureTestResult) *AzureLastTestView {
	if r == nil {
		return nil
	}
	return &AzureLastTestView{OK: r.OK, LatencyMs: r.LatencyMs, Error: r.Error, At: azureStamp(r.At)}
}

// azureOrgInfo names the organisation a shared entry belongs to.
type azureOrgInfo struct {
	id   string
	name string
}

// connectionView builds a resource's view. org is set for an
// organisation's resource; readOnly marks one listed in user scope.
func connectionView(rec storageif.AzureConnectionRecord, cred azureCredential, modelCount int, org *azureOrgInfo, readOnly bool) AzureConnectionView {
	v := AzureConnectionView{
		ID:           rec.ID,
		Name:         rec.Name,
		Legacy:       rec.ID == ai.AzureDefaultConnectionID,
		Status:       cred.status,
		StatusReason: cred.reason,
		ModelCount:   modelCount,
		LastTest:     lastTestView(rec.LastTest),
		UpdatedAt:    azureStamp(rec.UpdatedAt),
	}
	if org != nil {
		v.Shared, v.OrgID, v.OrgName = true, org.id, org.name
	}
	if cred.parsed {
		v.Config = AzureConnectionConfigView{
			Endpoint:        cred.cfg.Endpoint,
			AuthMode:        cred.cfg.Auth.Mode,
			HasSecret:       cred.cfg.HasSecret(),
			TenantID:        cred.cfg.Auth.TenantID,
			ClientID:        cred.cfg.Auth.ClientID,
			ProjectEndpoint: cred.cfg.ProjectEndpoint,
		}
		v.CanDiscover = cred.cfg.ProjectAccess()
		v.SupportsAgents = cred.cfg.ProjectAccess()
		v.SuggestDiscovery = v.CanDiscover && rec.DiscoveryDismissedAt == nil && !readOnly
	}
	return v
}

// modelTarget is a record's routed id (its deployment name for an
// embedding model).
func modelTarget(m storageif.AzureModelRecord) string {
	if t, ok := ai.AzureModelTarget(m.Kind, m.Deployment); ok {
		return t
	}
	return m.Deployment
}

// modelStatus is a model's status on its resource: the resource's, or
// unsupported for an agent on a resource that cannot run one.
func modelStatus(m storageif.AzureModelRecord, cred azureCredential) (string, string) {
	if !cred.ready() {
		return cred.status, cred.reason
	}
	if m.Kind == storageif.AzureKindAgent && !cred.cfg.ProjectAccess() {
		return AzureStatusUnsupported, azureReasonAgentUnsupported
	}
	return AzureStatusReady, ""
}

func modelView(m storageif.AzureModelRecord, connName string, cred azureCredential, reported int, org *azureOrgInfo) AzureModelView {
	status, reason := modelStatus(m, cred)
	v := AzureModelView{
		ID:                    m.ID,
		ModelID:               ai.RegistryModelID(m.ID),
		ConnectionID:          m.ConnectionID,
		ConnectionName:        connName,
		Kind:                  m.Kind,
		Deployment:            m.Deployment,
		Target:                modelTarget(m),
		DisplayName:           m.DisplayName,
		Purpose:               m.Purpose,
		PurposeCustom:         m.PurposeCustom,
		Description:           m.Description,
		ContextWindow:         m.ContextWindow,
		ReportedContextWindow: reported,
		MaxOutputTokens:       m.MaxOutputTokens,
		IsDefault:             m.IsDefault,
		IsKnowledgeEmbedding:  m.IsKnowledgeEmbedding,
		Status:                status,
		StatusReason:          reason,
		LastTest:              lastTestView(m.LastTest),
		UpdatedAt:             azureStamp(m.UpdatedAt),
	}
	if org != nil {
		v.Shared, v.OrgID, v.OrgName = true, org.id, org.name
	}
	return v
}

// ---- Sort order (no sort_order column) ----

// azurePurposeRank is a preset's place in the fixed purpose order; custom
// purposes come after the presets (then A–Z by their text).
func azurePurposeRank(purpose string) int {
	for i, p := range storageif.AzurePurposePresets {
		if p == purpose {
			return i
		}
	}
	return len(storageif.AzurePurposePresets)
}

// azureEntryLess orders entries for display: purpose rank (custom purposes
// A–Z after the presets), then personal before shared, then display name
// (case-insensitively), then id.
func azureEntryLess(purposeA, customA string, sharedA bool, nameA, idA, purposeB, customB string, sharedB bool, nameB, idB string) bool {
	if ra, rb := azurePurposeRank(purposeA), azurePurposeRank(purposeB); ra != rb {
		return ra < rb
	}
	if ca, cb := strings.ToLower(customA), strings.ToLower(customB); ca != cb {
		return ca < cb
	}
	if sharedA != sharedB {
		return !sharedA
	}
	if na, nb := strings.ToLower(nameA), strings.ToLower(nameB); na != nb {
		return na < nb
	}
	return idA < idB
}

func sortModelViews(v []AzureModelView) {
	sort.SliceStable(v, func(i, j int) bool {
		a, b := v[i], v[j]
		return azureEntryLess(a.Purpose, a.PurposeCustom, a.Shared, a.DisplayName, a.ID, b.Purpose, b.PurposeCustom, b.Shared, b.DisplayName, b.ID)
	})
}

// ---- Registry reads ----

// azureOwnerState is one owner's registry as the views need it: its live
// (non-pending) resources and models, and each resource's credential.
type azureOwnerState struct {
	owner  storageif.AzureOwner
	scope  string
	org    *azureOrgInfo
	conns  []storageif.AzureConnectionRecord
	models []storageif.AzureModelRecord
	creds  map[string]azureCredential
	// importPending: the owner's legacy document is not in the registry yet
	// (ensureLegacyImported), so conns and models are not the whole story.
	importPending bool
}

func (st *azureOwnerState) connection(id string) (storageif.AzureConnectionRecord, bool) {
	for _, c := range st.conns {
		if c.ID == id {
			return c, true
		}
	}
	return storageif.AzureConnectionRecord{}, false
}

func (st *azureOwnerState) connName(id string) string {
	if c, ok := st.connection(id); ok {
		return c.Name
	}
	return ""
}

// entriesOf is resource connID's full entry set, as the provider takes it.
func (st *azureOwnerState) entriesOf(connID string) []ai.AzureRegisteredModel {
	return registeredEntries(st.models, connID)
}

func registeredModel(m storageif.AzureModelRecord) ai.AzureRegisteredModel {
	return ai.AzureRegisteredModel{EntryID: m.ID, Kind: m.Kind, Deployment: m.Deployment, ContextWindow: m.ContextWindow, MaxOutputTokens: m.MaxOutputTokens}
}

func registeredEntries(models []storageif.AzureModelRecord, connID string) []ai.AzureRegisteredModel {
	out := []ai.AzureRegisteredModel{}
	for _, m := range models {
		if m.ConnectionID == connID {
			out = append(out, registeredModel(m))
		}
	}
	return out
}

// loadOwner reads owner's live registry — pending resources (a create still
// in flight, or one an earlier process left behind) and their models are
// left out — and, with creds, each resource's credential.
func (s *AzureModelService) loadOwner(ctx context.Context, owner storageif.AzureOwner, scope string, org *azureOrgInfo, creds bool) (*azureOwnerState, error) {
	conns, err := s.registry.ListConnections(ctx, owner)
	if err != nil {
		return nil, err
	}
	models, err := s.registry.ListModels(ctx, owner)
	if err != nil {
		return nil, err
	}
	st := &azureOwnerState{owner: owner, scope: scope, org: org, creds: map[string]azureCredential{}}
	live := map[string]bool{}
	for _, c := range conns {
		if c.Pending {
			continue
		}
		live[c.ID] = true
		st.conns = append(st.conns, c)
	}
	for _, m := range models {
		if live[m.ConnectionID] {
			st.models = append(st.models, m)
		}
	}
	if creds {
		for _, c := range st.conns {
			cred, err := s.credentialState(owner, scope, c.ID)
			if err != nil {
				return nil, err
			}
			st.creds[c.ID] = cred
		}
	}
	return st, nil
}

// sharedOrgs lists the organisations whose shared registries the caller
// sees in user scope: every organisation where they are an admin or a member
// (viewers and guests run on no shared credential and see none).
func (s *AzureModelService) sharedOrgs(ctx context.Context, scope string) ([]azureOrgInfo, error) {
	if !s.OrgRegistriesAvailable() || s.orgs == nil || scope == "" {
		return nil, nil
	}
	list, err := s.orgs.ListOrgsForUser(ctx, scope)
	if err != nil {
		return nil, fmt.Errorf("list the caller's organisations: %w", err)
	}
	var out []azureOrgInfo
	for _, o := range list {
		if o == nil {
			continue
		}
		if azureMemberRole(o, scope) {
			out = append(out, azureOrgInfo{id: o.ID, name: o.Name})
		}
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].name) < strings.ToLower(out[j].name) })
	return out, nil
}

// azureMemberRole reports whether userID is an admin or member of o (the
// roles that run on the organisation's shared credentials).
func azureMemberRole(o *storageif.Organisation, userID string) bool {
	for _, m := range o.Members {
		if m.UserID == userID {
			return OrgRoleUsesSharedCredential(m.Role)
		}
	}
	return false
}

// orgInfo names orgID (its id alone when the directory cannot).
func (s *AzureModelService) orgInfo(ctx context.Context, orgID string) *azureOrgInfo {
	info := &azureOrgInfo{id: orgID}
	if s.orgs != nil {
		if o, err := s.orgs.LoadOrg(ctx, orgID); err == nil && o != nil {
			info.name = o.Name
		}
	}
	return info
}

// userLabel renders a user id as members see it: the display name, else
// the email, else the id.
func (s *AzureModelService) userLabel(ctx context.Context, userID string) string {
	if userID == "" || s.users == nil {
		return userID
	}
	u, err := s.users.LoadUserByID(ctx, userID)
	if err != nil || u == nil {
		return userID
	}
	if u.DisplayName != "" {
		return u.DisplayName
	}
	if u.Email != "" {
		return u.Email
	}
	return userID
}

// orgCredentialMeta fills a shared resource's lender and (for org scope) its
// credential's compare-and-swap token.
func (s *AzureModelService) orgCredentialMeta(ctx context.Context, v *AzureConnectionView, orgID string, withToken bool) {
	if s.orgKeys == nil {
		return
	}
	meta, ok, err := s.orgKeys.OrgKeyMeta(orgID, credentialKey(v.ID))
	if err != nil || !ok {
		return
	}
	lender := meta.SecretSetBy
	if lender == "" {
		lender = meta.UpdatedBy // a row no save has stamped since v24
	}
	v.SharedByID = lender
	v.SharedBy = s.userLabel(ctx, lender)
	if withToken && !meta.UpdatedAt.IsZero() {
		v.CredentialUpdatedAt = formatOrgKeyStamp(meta.UpdatedAt)
	}
}

// connectionViews builds the views of one owner's resources.
func (s *AzureModelService) connectionViews(ctx context.Context, st *azureOwnerState, readOnly bool) []AzureConnectionView {
	counts := map[string]int{}
	for _, m := range st.models {
		counts[m.ConnectionID]++
	}
	out := make([]AzureConnectionView, 0, len(st.conns))
	for _, c := range st.conns {
		v := connectionView(c, st.creds[c.ID], counts[c.ID], st.org, readOnly)
		if st.owner.IsOrg() {
			s.orgCredentialMeta(ctx, &v, st.owner.ID, !readOnly)
		}
		out = append(out, v)
	}
	return out
}

// ---- Resources ----

// ListConnections is GET /connections: the scope's resources (user scope:
// the caller's own, then the shared resources of every organisation where
// they are an admin or member, read-only; org scope: that organisation's),
// the registry's state, the limits and whether the knowledge-base
// designation is offered. Network-free. Runs the legacy import, the desktop's
// pending-row rollback and (hourly) the orphan sweep for the owner.
func (s *AzureModelService) ListConnections(ctx context.Context, as AzureScope) (out AzureConnectionsListing, err error) {
	defer logger.Guard("AzureModels.ListConnections", &err)
	owner, err := s.ownerFor(as)
	if err != nil {
		return AzureConnectionsListing{}, err
	}
	s.prepareOwner(ctx, owner, as.Scope)
	var org *azureOrgInfo
	if owner.IsOrg() {
		org = s.orgInfo(ctx, as.OrgID)
	}
	st, err := s.loadOwner(ctx, owner, as.Scope, org, true)
	if err != nil {
		return AzureConnectionsListing{}, err
	}
	s.sweepOrphans(ctx, st)
	out.Connections = s.connectionViews(ctx, st, false)
	if !owner.IsOrg() {
		shared, err := s.sharedStates(ctx, as.Scope)
		if err != nil {
			return AzureConnectionsListing{}, err
		}
		for _, sst := range shared {
			out.Connections = append(out.Connections, s.connectionViews(ctx, sst, true)...)
		}
	}
	state := s.registry.State(ctx)
	out.RegistryState = string(state.State)
	if out.RegistryState == "" {
		out.RegistryState = string(storageif.AzureRegistryOK)
	}
	if state.State == storageif.AzureRegistryRecovered {
		out.RecoveredCopyPath = state.QuarantinePath
	}
	out.Limits = AzureRegistryLimits{Connections: storageif.AzureMaxConnections, Models: storageif.AzureMaxModels}
	out.KnowledgeEmbeddingEnabled = s.knowledgeEmbeddingEnabled
	return out, nil
}

// sharedStates loads the shared registries user scope lists (see
// sharedOrgs), each imported first.
func (s *AzureModelService) sharedStates(ctx context.Context, scope string) ([]*azureOwnerState, error) {
	orgs, err := s.sharedOrgs(ctx, scope)
	if err != nil {
		return nil, err
	}
	var out []*azureOwnerState
	for i := range orgs {
		o := orgs[i]
		owner := storageif.AzureOrgOwner(o.id)
		pending := !s.ensureLegacyImported(ctx, owner, "")
		st, err := s.loadOwner(ctx, owner, "", &o, true)
		if err != nil {
			return nil, err
		}
		st.importPending = pending
		out = append(out, st)
	}
	return out, nil
}

// prepareOwner runs what every registry read path runs first: the legacy
// import, and on the desktop the rollback of pending rows an earlier
// process left behind. It reports whether owner's legacy import is still
// PENDING (see ensureLegacyImported): the read paths that answer for the
// legacy document — the server default, raw ids, the member-first alias
// rule, the knowledge base's embedder — must not answer as if it were not
// there.
func (s *AzureModelService) prepareOwner(ctx context.Context, owner storageif.AzureOwner, scope string) (importPending bool) {
	importPending = !s.ensureLegacyImported(ctx, owner, scope)
	s.rollbackStalePending(ctx, owner, scope)
	return importPending
}

// GetConnection is GET /connections/{id} (the scope's own resources only).
func (s *AzureModelService) GetConnection(ctx context.Context, as AzureScope, id string) (out AzureConnectionView, err error) {
	defer logger.Guard("AzureModels.GetConnection", &err)
	owner, err := s.ownerFor(as)
	if err != nil {
		return AzureConnectionView{}, err
	}
	s.prepareOwner(ctx, owner, as.Scope)
	return s.viewConnection(ctx, owner, as, id)
}

// viewConnection reads one live resource of owner and builds its view.
func (s *AzureModelService) viewConnection(ctx context.Context, owner storageif.AzureOwner, as AzureScope, id string) (AzureConnectionView, error) {
	rec, err := s.liveConnection(ctx, owner, id)
	if err != nil {
		return AzureConnectionView{}, err
	}
	cred, err := s.credentialState(owner, as.Scope, id)
	if err != nil {
		return AzureConnectionView{}, err
	}
	models, err := s.registry.ListModels(ctx, owner)
	if err != nil {
		return AzureConnectionView{}, err
	}
	n := 0
	for _, m := range models {
		if m.ConnectionID == id {
			n++
		}
	}
	var org *azureOrgInfo
	if owner.IsOrg() {
		org = s.orgInfo(ctx, owner.ID)
	}
	v := connectionView(rec, cred, n, org, false)
	if owner.IsOrg() {
		s.orgCredentialMeta(ctx, &v, owner.ID, true)
	}
	return v, nil
}

// liveConnection is GetConnection with a pending row reading as absent.
func (s *AzureModelService) liveConnection(ctx context.Context, owner storageif.AzureOwner, id string) (storageif.AzureConnectionRecord, error) {
	if !ai.ValidAzureConnectionID(id) {
		return storageif.AzureConnectionRecord{}, storageif.ErrAzureConnectionNotFound
	}
	rec, err := s.registry.GetConnection(ctx, owner, id)
	if err != nil {
		return storageif.AzureConnectionRecord{}, err
	}
	if rec.Pending {
		return storageif.AzureConnectionRecord{}, storageif.ErrAzureConnectionNotFound
	}
	return rec, nil
}

// AzureConnectionConfigInput is a resource's settings as a client sends
// them: the endpoint, the credential (a missing secret keeps the stored one
// where the carry rules allow) and the optional project endpoint. It has no
// deployment fields: a resource's models are registry entries.
type AzureConnectionConfigInput struct {
	Endpoint        string              `json:"endpoint"`
	Auth            ai.AzureFoundryAuth `json:"auth"`
	ProjectEndpoint string              `json:"projectEndpoint,omitempty"`
}

// document is the input as a (normalised) credential document.
func (in AzureConnectionConfigInput) document() ai.AzureFoundryConfig {
	cfg := ai.AzureFoundryConfig{Endpoint: in.Endpoint, Auth: in.Auth, ProjectEndpoint: in.ProjectEndpoint}
	cfg.Normalize()
	return cfg
}

// AzureConnectionInput is POST /connections.
type AzureConnectionInput struct {
	// Name is the resource's display name; "" takes the endpoint host's
	// first label.
	Name   string                     `json:"name"`
	Config AzureConnectionConfigInput `json:"config"`
}

// AzureConnectionPatch is PUT /connections/{id}: every field optional.
type AzureConnectionPatch struct {
	Name   *string                     `json:"name,omitempty"`
	Config *AzureConnectionConfigInput `json:"config,omitempty"`
	// UpdatedAt is the resource's updatedAt as last read: a stale value is
	// 409 CONFLICT (compare-and-swap on the registry row).
	UpdatedAt string `json:"updatedAt,omitempty"`
	// CredentialUpdatedAt (org scope) is the credentialUpdatedAt last read,
	// sent with a credential change: the compare-and-swap on the org's
	// stored credential.
	CredentialUpdatedAt string `json:"credentialUpdatedAt,omitempty"`
	// DismissDiscovery hides (true) or brings back (false) the "Found in
	// your project" prompt.
	DismissDiscovery *bool `json:"dismissDiscovery,omitempty"`
	// Force confirms a re-index: the endpoint change of a resource holding
	// the knowledge-base model of a non-empty corpus (else 409
	// REINDEX_REQUIRED).
	Force bool `json:"force,omitempty"`
}

// AzureDeleteOptions are DELETE /connections/{id}'s flags.
type AzureDeleteOptions struct {
	// Cascade deletes the resource's models too (else a resource with models
	// is 409 CONNECTION_IN_USE).
	Cascade bool
	// Force confirms the re-index a deletion of the knowledge-base model
	// needs on a non-empty corpus (else 409 REINDEX_REQUIRED).
	Force bool
}

// validateConnectionDocument validates a resource's document for a save or
// a draft test: an organisation's resource never signs in with the Azure
// CLI (a shared credential runs on every member's machine); no chat
// deployment is required; deployment fields are refused except on the
// default resource. An INVALID_AUTH or MISSING_SECRET refusal names its
// field (details.field).
func validateConnectionDocument(cfg ai.AzureFoundryConfig, connID string, org bool) error {
	if org && cfg.Auth.Mode == ai.AzureAuthModeEntraCLI {
		return azureRegErr(http.StatusBadRequest, ai.AzureCfgErrInvalidAuth,
			"the Azure CLI sign-in mode cannot be shared with an organisation — use an API key or a Microsoft Entra ID service principal",
			&ai.AzureConfigError{Code: ai.AzureCfgErrInvalidAuth, Message: "the Azure CLI sign-in mode cannot be shared with an organisation"},
			map[string]any{"field": "mode"})
	}
	return withAuthField(cfg, ai.ValidateAzureConnection(cfg, connID != ai.AzureDefaultConnectionID))
}

// withAuthField wraps an INVALID_AUTH or MISSING_SECRET refusal of cfg with
// the auth field it is about ({field: mode | key | tenantId | clientId |
// clientSecret}), so a client routes it to that field without reading the
// message. Anything else is returned as is.
func withAuthField(cfg ai.AzureFoundryConfig, err error) error {
	var ce *ai.AzureConfigError
	if err == nil || !errors.As(err, &ce) || (ce.Code != ai.AzureCfgErrInvalidAuth && ce.Code != ai.AzureCfgErrMissingSecret) {
		return err
	}
	return azureRegErr(http.StatusBadRequest, ce.Code, ce.Message, err, map[string]any{"field": azureAuthField(cfg, ce)})
}

// azureAuthField finds the auth field ce is about. ai's validation reports
// the first failing field, so a field is the culprit when substituting a
// valid value for it changes the refusal.
func azureAuthField(cfg ai.AzureFoundryConfig, ce *ai.AzureConfigError) string {
	const validGUID = "00000000-0000-0000-0000-000000000000"
	switch cfg.Auth.Mode {
	case ai.AzureAuthModeAPIKey:
		return "key"
	case ai.AzureAuthModeEntra, ai.AzureAuthModeEntraCLI:
	default:
		return "mode"
	}
	if ce.Code == ai.AzureCfgErrMissingSecret {
		return "clientSecret"
	}
	changes := func(mut func(*ai.AzureFoundryConfig)) bool {
		c := cfg
		mut(&c)
		err := ai.ValidateAzureConnection(c, false)
		return err == nil || err.Error() != ce.Message
	}
	if cfg.Auth.Mode == ai.AzureAuthModeEntraCLI {
		if !changes(func(c *ai.AzureFoundryConfig) { c.Auth.TenantID, c.Auth.ClientID, c.Auth.ClientSecret = "", "", "" }) {
			return "mode" // refused whatever it carries: the mode itself
		}
		if changes(func(c *ai.AzureFoundryConfig) { c.Auth.TenantID = "" }) {
			return "tenantId"
		}
		if cfg.Auth.ClientID != "" {
			return "clientId"
		}
		return "clientSecret"
	}
	if changes(func(c *ai.AzureFoundryConfig) { c.Auth.TenantID = validGUID }) {
		return "tenantId"
	}
	if changes(func(c *ai.AzureFoundryConfig) { c.Auth.ClientID = validGUID }) {
		return "clientId"
	}
	return "clientSecret"
}

// connectionNameFromHost is a resource's default name: its endpoint host's
// first label ("my-res" for https://my-res.openai.azure.com).
func connectionNameFromHost(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil {
		return ""
	}
	label, _, _ := strings.Cut(u.Hostname(), ".")
	return cleanAzureText(label)
}

// validateConnectionName cleans and checks a resource name.
func validateConnectionName(name string) (string, error) {
	clean := cleanAzureText(name)
	if !validAzureText(name, clean, 1, storageif.AzureNameMaxLen) {
		return "", azureRegErr(http.StatusBadRequest, AzureCodeInvalidConnectionName,
			fmt.Sprintf("the resource name must be 1 to %d characters, at least one of them a visible letter, digit, punctuation mark or symbol, and none of them a control, invisible (zero-width, bidi, filler) or unassigned character", storageif.AzureNameMaxLen), nil, nil)
	}
	return clean, nil
}

// newAzureRegistryID returns a random resource or model id: ten characters
// of [a-z0-9] (never "default").
func newAzureRegistryID() (string, error) {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	out := make([]byte, 0, 10)
	var buf [16]byte
	for len(out) < 10 {
		if _, err := rand.Read(buf[:]); err != nil {
			return "", fmt.Errorf("generate an id: %w", err)
		}
		for _, b := range buf {
			if b >= 252 { // 252 = 7*36: keeps the draw uniform
				continue
			}
			out = append(out, alphabet[b%36])
			if len(out) == 10 {
				break
			}
		}
	}
	return string(out), nil
}

// saveCredential stores a resource's document: the caller's own KeyStore
// entry, or the organisation's (compare-and-swap on expected, the stored
// credential's stamp — zero for a credential that must not exist yet;
// secretCarried keeps the stored secret's lender). Returns the org stamp.
func (s *AzureModelService) saveCredential(owner storageif.AzureOwner, scope, connID, raw, actor string, secretCarried bool, expected time.Time) (time.Time, error) {
	key := credentialKey(connID)
	if !owner.IsOrg() {
		return time.Time{}, s.secrets.Save(scope, key, raw)
	}
	at, ok, err := s.orgKeys.SaveOrgIfUnchanged(owner.ID, key, raw, actor, secretCarried, expected)
	if err != nil {
		return time.Time{}, err
	}
	if !ok {
		return time.Time{}, ErrOrgCredentialConflict
	}
	return at, nil
}

// deleteCredential removes a resource's stored document (a missing one is
// no error).
func (s *AzureModelService) deleteCredential(owner storageif.AzureOwner, scope, connID string) error {
	key := credentialKey(connID)
	var err error
	if owner.IsOrg() {
		err = s.orgKeys.DeleteOrg(owner.ID, key)
	} else {
		err = s.secrets.Delete(scope, key)
	}
	if errors.Is(err, storage.ErrSecretNotFound) {
		return nil
	}
	return err
}

// CreateConnection is POST /connections: validate, pre-check the limit and
// the name, save the credential, then — under the owner lock — re-check the
// limit and write the registry row. On the desktop (no transaction, no
// listable keyring) the row is written pending first, the keyring entry
// saved, then the row finalised. Never creates the "default" resource.
func (s *AzureModelService) CreateConnection(ctx context.Context, as AzureScope, in AzureConnectionInput) (out AzureConnectionView, err error) {
	defer logger.Guard("AzureModels.CreateConnection", &err)
	owner, err := s.ownerFor(as)
	if err != nil {
		return AzureConnectionView{}, err
	}
	cfg := in.Config.document()
	name := in.Name
	if strings.TrimSpace(name) == "" {
		name = connectionNameFromHost(cfg.Endpoint)
	}
	name, err = validateConnectionName(name)
	if err != nil {
		return AzureConnectionView{}, err
	}
	if err := validateConnectionDocument(cfg, "", owner.IsOrg()); err != nil {
		return AzureConnectionView{}, err
	}
	raw, err := cfg.Encode()
	if err != nil {
		return AzureConnectionView{}, err
	}
	s.prepareOwner(ctx, owner, as.Scope)
	conns, err := s.registry.ListConnections(ctx, owner)
	if err != nil {
		return AzureConnectionView{}, err
	}
	if err := s.checkNameFree(ctx, owner, conns, "", name); err != nil {
		return AzureConnectionView{}, err
	}
	if len(conns) >= storageif.AzureMaxConnections {
		return AzureConnectionView{}, tooManyConnections()
	}
	id, err := newAzureRegistryID()
	if err != nil {
		return AzureConnectionView{}, err
	}
	rec := storageif.AzureConnectionRecord{ID: id, Name: name}
	if s.registry.PendingWrites() {
		err = s.createConnectionPending(ctx, owner, as.Scope, rec, raw)
	} else {
		err = s.createConnectionTx(ctx, owner, as.Scope, rec, raw)
	}
	if err != nil {
		return AzureConnectionView{}, mapConnectionWriteErr(err)
	}
	return s.viewConnection(ctx, owner, as, id)
}

// createConnectionTx is the transactional create (Postgres, the fake): the
// credential first, then the row under the lock. A failed row write removes
// the credential it orphaned (best effort — the sweep is the backstop).
func (s *AzureModelService) createConnectionTx(ctx context.Context, owner storageif.AzureOwner, scope string, rec storageif.AzureConnectionRecord, raw string) error {
	if _, err := s.saveCredential(owner, scope, rec.ID, raw, scope, false, time.Time{}); err != nil {
		return err
	}
	err := s.registry.WithOwnerLock(ctx, owner, func(lctx context.Context) error {
		counts, err := s.registry.Counts(lctx, owner)
		if err != nil {
			return err
		}
		if counts.Connections >= storageif.AzureMaxConnections {
			return storageif.ErrAzureTooManyConnections
		}
		_, err = s.registry.CreateConnection(lctx, owner, rec)
		return err
	})
	if err != nil {
		if derr := s.deleteCredential(owner, scope, rec.ID); derr != nil {
			logger.Warn("azure registry: removing the credential of a resource that was not created failed", "owner", owner.String(), "resource", rec.ID, "error", derr)
		}
		return err
	}
	return nil
}

// createConnectionPending is the desktop's create: pending row, keyring
// entry, finalised row. A keyring failure removes the pending row.
func (s *AzureModelService) createConnectionPending(ctx context.Context, owner storageif.AzureOwner, scope string, rec storageif.AzureConnectionRecord, raw string) error {
	rec.Pending = true
	err := s.registry.WithOwnerLock(ctx, owner, func(lctx context.Context) error {
		counts, err := s.registry.Counts(lctx, owner)
		if err != nil {
			return err
		}
		if counts.Connections >= storageif.AzureMaxConnections {
			return storageif.ErrAzureTooManyConnections
		}
		_, err = s.registry.CreateConnection(lctx, owner, rec)
		return err
	})
	if err != nil {
		return err
	}
	if _, err := s.saveCredential(owner, scope, rec.ID, raw, scope, false, time.Time{}); err != nil {
		if _, derr := s.registry.DeleteConnection(ctx, owner, rec.ID); derr != nil {
			logger.Warn("azure registry: removing a pending resource after its keyring entry failed", "resource", rec.ID, "error", derr)
		}
		return err
	}
	return s.registry.FinalizeConnection(ctx, owner, rec.ID)
}

// checkConnectionName refuses a name another resource (other than skipID)
// has, case-insensitively.
func checkConnectionName(conns []storageif.AzureConnectionRecord, skipID, name string) error {
	for _, c := range conns {
		if c.ID != skipID && strings.EqualFold(c.Name, name) {
			return azureRegErr(http.StatusConflict, AzureCodeDuplicateConnectionName,
				fmt.Sprintf("a resource named %q already exists", c.Name), storageif.ErrAzureDuplicateConnectionName, nil)
		}
	}
	return nil
}

// connectionInUse refuses the delete of a resource that still has n models
// without cascade.
func connectionInUse(n int) error {
	return azureRegErr(http.StatusConflict, AzureCodeConnectionInUse,
		fmt.Sprintf("the resource still has %d model(s) — remove them too, or remove them first", n), nil, map[string]any{"modelCount": n})
}

func tooManyConnections() error {
	return azureRegErr(http.StatusConflict, AzureCodeTooManyConnections,
		fmt.Sprintf("you can add at most %d Azure AI Foundry resources", storageif.AzureMaxConnections),
		storageif.ErrAzureTooManyConnections, map[string]any{"limit": storageif.AzureMaxConnections})
}

func tooManyModels() error {
	return azureRegErr(http.StatusConflict, AzureCodeTooManyModels,
		fmt.Sprintf("you can add at most %d Azure AI Foundry models", storageif.AzureMaxModels),
		storageif.ErrAzureTooManyModels, map[string]any{"limit": storageif.AzureMaxModels})
}

// mapConnectionWriteErr gives the store's resource refusals their copy.
func mapConnectionWriteErr(err error) error {
	var re *AzureRegistryError
	switch {
	case errors.As(err, &re):
		return err
	case errors.Is(err, storageif.ErrAzureTooManyConnections):
		return tooManyConnections()
	case errors.Is(err, storageif.ErrAzureDuplicateConnectionName):
		return azureRegErr(http.StatusConflict, AzureCodeDuplicateConnectionName, "another resource already has this name", err, nil)
	case errors.Is(err, storageif.ErrAzureRegistryConflict):
		return azureRegErr(http.StatusConflict, AzureCodeConflict, "the resource changed since it was loaded — reload it and apply your edit again", err, nil)
	}
	return err
}

// UpdateConnection is PUT /connections/{id}: rename it (compare-and-swap on
// updatedAt), change its credential — the omitted secret carried by the host
// rules, the default resource's legacy deployment fields carried unchanged,
// an organisation's credential saved compare-and-swap on
// credentialUpdatedAt — and/or dismiss its discovery prompt. An endpoint
// change of the resource that holds the knowledge-base model on a non-empty
// corpus is 409 REINDEX_REQUIRED unless Force.
//
// Two stores, one change: every check the registry write can fail on runs
// BEFORE the credential is saved — the row's compare-and-swap token, the
// name under the registry store's own uniqueness rule (Postgres lower(),
// which Go's case folding cannot stand in for), the credential's own token
// — then the credential is saved and the row written (rename or touch, and
// the dismissal) in one locked write. Should that write still fail (a race,
// a store failure), or anything after the save, the previous credential goes
// back (an organisation's with its writer, lender and stamp) and the error
// is returned — the request changed nothing; when it cannot go back the
// error is an AzureCredentialChangedError, which the handler audits. On
// success the replaced credential's process state is released.
func (s *AzureModelService) UpdateConnection(ctx context.Context, as AzureScope, id string, in AzureConnectionPatch) (out AzureConnectionView, err error) {
	defer logger.Guard("AzureModels.UpdateConnection", &err)
	owner, err := s.ownerFor(as)
	if err != nil {
		return AzureConnectionView{}, err
	}
	expected, err := parseAzureStamp(in.UpdatedAt)
	if err != nil {
		return AzureConnectionView{}, err
	}
	var name string
	if in.Name != nil {
		if name, err = validateConnectionName(*in.Name); err != nil {
			return AzureConnectionView{}, err
		}
	}
	s.prepareOwner(ctx, owner, as.Scope)
	rec, err := s.liveConnection(ctx, owner, id)
	if err != nil {
		return AzureConnectionView{}, err
	}
	if !expected.IsZero() && !expected.Truncate(time.Microsecond).Equal(rec.UpdatedAt) {
		return AzureConnectionView{}, mapConnectionWriteErr(storageif.ErrAzureRegistryConflict)
	}
	if in.Name != nil {
		conns, err := s.registry.ListConnections(ctx, owner)
		if err != nil {
			return AzureConnectionView{}, err
		}
		if err := s.checkNameFree(ctx, owner, conns, id, name); err != nil {
			return AzureConnectionView{}, err
		}
	}
	var rot *azureRotation
	if in.Config != nil {
		if rot, err = s.prepareRotation(ctx, owner, as, id, *in.Config, in.CredentialUpdatedAt, in.Force); err != nil {
			return AzureConnectionView{}, err
		}
		if err := rot.save(s, owner, as.Scope, id); err != nil {
			return AzureConnectionView{}, err
		}
	}
	// From here on the new credential is stored: a failure puts the
	// previous one back.
	failed := func(cause error) (AzureConnectionView, error) {
		if rot == nil {
			return AzureConnectionView{}, cause
		}
		return AzureConnectionView{}, s.undoCredentialSave(owner, as.Scope, id, rot.snap, rot.savedAt, rot.forgetSaved, cause)
	}
	if in.Name != nil || in.Config != nil || in.DismissDiscovery != nil {
		err = s.registry.WithOwnerLock(ctx, owner, func(lctx context.Context) error {
			if in.Name != nil || in.Config != nil {
				if _, err := s.registry.UpdateConnection(lctx, owner, id, storageif.AzureConnectionUpdate{Name: name, ExpectedUpdatedAt: expected}); err != nil {
					return err
				}
			}
			if in.DismissDiscovery != nil {
				if _, err := s.registry.SetDiscoveryDismissed(lctx, owner, id, *in.DismissDiscovery); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return failed(mapConnectionWriteErr(err))
		}
	}
	// Inside a request the row commits with the answer: an error here rolls
	// it back, so the credential must go back with it.
	v, err := s.viewConnection(ctx, owner, as, id)
	if err != nil {
		return failed(err)
	}
	if rot != nil {
		rot.release()
	}
	return v, nil
}

// checkNameFree refuses (DUPLICATE_CONNECTION_NAME) a name another resource
// of owner (other than skipID) has: among conns case-insensitively as Go
// folds case (the copy names the resource), and under the registry store's
// own uniqueness rule, which decides what its write will refuse — asked
// before a credential is saved, so a name the store refuses writes nothing.
func (s *AzureModelService) checkNameFree(ctx context.Context, owner storageif.AzureOwner, conns []storageif.AzureConnectionRecord, skipID, name string) error {
	if err := checkConnectionName(conns, skipID, name); err != nil {
		return err
	}
	taken, err := s.registry.ConnectionNameTaken(ctx, owner, name, skipID)
	if err != nil {
		return err
	}
	if taken {
		return azureRegErr(http.StatusConflict, AzureCodeDuplicateConnectionName,
			fmt.Sprintf("another resource already has the name %q (names are compared ignoring case)", name), storageif.ErrAzureDuplicateConnectionName, nil)
	}
	return nil
}

// azureRotation is a validated credential change of one resource
// (UpdateConnection's config): the document to save, the one it replaces
// and the snapshot that puts it back.
type azureRotation struct {
	cfg      ai.AzureFoundryConfig
	raw      string
	prev     ai.AzureFoundryConfig
	hadPrev  bool
	supplied bool
	// expected is the organisation credential's compare-and-swap token.
	expected time.Time
	snap     azureCredentialSnapshot
	// savedAt is the organisation save's new stamp (set by save).
	savedAt time.Time
}

// prepareRotation validates a resource's new document (see UpdateConnection)
// and snapshots the stored one — nothing is written. An organisation's
// credential that changed since the admin's read (another admin saved, or
// the lender left) is ErrOrgCredentialConflict here already.
func (s *AzureModelService) prepareRotation(ctx context.Context, owner storageif.AzureOwner, as AzureScope, id string, in AzureConnectionConfigInput, credentialUpdatedAt string, force bool) (*azureRotation, error) {
	cfg := in.document()
	credExpected, err := parseOrgKeyStamp(credentialUpdatedAt)
	if err != nil {
		return nil, err
	}
	if owner.IsOrg() && cfg.Auth.Mode == ai.AzureAuthModeEntraCLI {
		return nil, validateConnectionDocument(cfg, id, true)
	}
	if owner.IsOrg() && credExpected.IsZero() && cfg.HasSecret() {
		// A repair of an unreadable stored credential (see
		// SaveOrgAzureFoundryConfig): its own stamp is the token.
		stamp, repair, rerr := unreadableOrgStamp(s.orgKeys, owner.ID, credentialKey(id))
		if rerr != nil {
			return nil, rerr
		}
		if repair {
			credExpected = stamp
		}
	}
	prev, hadPrev := storedAzureConfig(func() (string, error) { return s.readCredential(owner, as.Scope, id) })
	supplied := cfg.HasSecret()
	validate := func(c ai.AzureFoundryConfig) error { return validateConnectionDocument(c, id, owner.IsOrg()) }
	if refusal := carryAzureSecret(&cfg, prev, hadPrev); refusal != nil {
		return nil, withAuthField(cfg, saveCarryErrorWith(cfg, refusal, validate))
	}
	if err := validate(cfg); err != nil {
		return nil, err
	}
	if hadPrev && id == ai.AzureDefaultConnectionID {
		// The legacy document keeps its deployment fields, unchanged, for a
		// binary rolled back to the single-document release.
		cfg.ChatDeployment, cfg.EmbeddingDeployment = prev.ChatDeployment, prev.EmbeddingDeployment
		cfg.Deployments, cfg.ClaudeDeployments = prev.Deployments, prev.ClaudeDeployments
	}
	if hadPrev && !force && prev.Endpoint != cfg.Endpoint {
		if err := s.checkReindexForConnection(ctx, owner, as.Scope, id); err != nil {
			return nil, err
		}
	}
	raw, err := cfg.Encode()
	if err != nil {
		return nil, err
	}
	if owner.IsOrg() && credExpected.IsZero() && hadPrev {
		// An org credential that exists is only replaced with its token.
		return nil, ErrOrgCredentialConflict
	}
	snap, err := s.snapshotCredential(owner, as.Scope, id)
	if err != nil {
		return nil, err
	}
	if owner.IsOrg() && !snap.stamp().Equal(credExpected) {
		// Its compare-and-swap would refuse the save anyway: refuse before it.
		return nil, ErrOrgCredentialConflict
	}
	return &azureRotation{cfg: cfg, raw: raw, prev: prev, hadPrev: hadPrev, supplied: supplied, expected: credExpected, snap: snap}, nil
}

// save stores the rotation's document (an organisation's compare-and-swap
// on its token; a carried secret keeps its lender).
func (r *azureRotation) save(s *AzureModelService, owner storageif.AzureOwner, scope, id string) error {
	at, err := s.saveCredential(owner, scope, id, r.raw, scope, !r.supplied, r.expected)
	if err != nil {
		return err
	}
	r.savedAt = at
	return nil
}

// release drops what the process kept for the replaced credential once the
// change went through, and the new resource's cached listing.
func (r *azureRotation) release() {
	ai.InvalidateAzureFoundryModels(r.cfg)
	if r.hadPrev && r.prev.Fingerprint() != r.cfg.Fingerprint() {
		ai.ForgetAzureFoundryCredential(r.prev)
	}
}

// forgetSaved drops what the process may have kept for the new credential
// while it was stored, once the previous one is back.
func (r *azureRotation) forgetSaved() {
	if !r.hadPrev || r.prev.Fingerprint() != r.cfg.Fingerprint() {
		ai.ForgetAzureFoundryCredential(r.cfg)
	}
}

// DeleteConnection is DELETE /connections/{id}: the row and — with Cascade
// — its models go in the (request) transaction; the credential only after
// the commit, and the process state kept for it with it. A resource with
// models is 409 CONNECTION_IN_USE without Cascade; one holding the
// knowledge-base model of a non-empty corpus is 409 REINDEX_REQUIRED
// without Force.
func (s *AzureModelService) DeleteConnection(ctx context.Context, as AzureScope, id string, opts AzureDeleteOptions) (out AzureConnectionDeleteResult, err error) {
	defer logger.Guard("AzureModels.DeleteConnection", &err)
	owner, err := s.ownerFor(as)
	if err != nil {
		return AzureConnectionDeleteResult{}, err
	}
	s.prepareOwner(ctx, owner, as.Scope)
	return s.deleteConnection(ctx, owner, as.Scope, id, opts)
}

// deleteConnection is DeleteConnection for owner (see it).
func (s *AzureModelService) deleteConnection(ctx context.Context, owner storageif.AzureOwner, scope, id string, opts AzureDeleteOptions) (AzureConnectionDeleteResult, error) {
	if _, err := s.liveConnection(ctx, owner, id); err != nil {
		return AzureConnectionDeleteResult{}, err
	}
	models, err := s.registry.ListModels(ctx, owner)
	if err != nil {
		return AzureConnectionDeleteResult{}, err
	}
	n, holdsKB := 0, false
	for _, m := range models {
		if m.ConnectionID == id {
			n++
			holdsKB = holdsKB || m.IsKnowledgeEmbedding
		}
	}
	if n > 0 && !opts.Cascade {
		return AzureConnectionDeleteResult{}, connectionInUse(n)
	}
	if holdsKB && !opts.Force {
		if err := s.checkReindex(ctx, owner, scope); err != nil {
			return AzureConnectionDeleteResult{}, err
		}
	}
	prev, hadPrev := storedAzureConfig(func() (string, error) { return s.readCredential(owner, scope, id) })
	// The credential as it is now: the delete after the commit takes only
	// this one — a credential saved again meanwhile (a single-document save
	// of resource "default", a racing rotation) is not this delete's.
	stamp, stamped := s.credentialStamp(owner, scope, id)
	var deleted []storageif.AzureModelRecord
	err = s.registry.WithOwnerLock(ctx, owner, func(lctx context.Context) error {
		// Re-checked under the lock: a model added, or the knowledge-base
		// designation moved onto this resource, since the checks above.
		models, err := s.registry.ListModels(lctx, owner)
		if err != nil {
			return err
		}
		n2, holdsKB2 := 0, false
		for _, m := range models {
			if m.ConnectionID == id {
				n2++
				holdsKB2 = holdsKB2 || m.IsKnowledgeEmbedding
			}
		}
		switch {
		case n2 > 0 && !opts.Cascade:
			return connectionInUse(n2)
		case holdsKB2 && !holdsKB && !opts.Force:
			// The re-index check cannot run under the lock: reload, decide.
			return storageif.ErrAzureRegistryConflict
		}
		deleted, err = s.registry.DeleteConnection(lctx, owner, id)
		return err
	})
	if err != nil {
		return AzureConnectionDeleteResult{}, mapConnectionWriteErr(err)
	}
	// After the commit — the request's when there is one (a rollback then
	// keeps the secret with its surviving row), else the owner lock's own,
	// which has committed by now: a rolled-back delete never strands a row
	// without its secret.
	storagedb.RegisterPostCommit(ctx, func() {
		if _, err := s.deleteCredentialIfUnchanged(owner, scope, id, stamp, stamped); err != nil {
			logger.Warn("azure registry: deleting a removed resource's credential failed (the orphan sweep retries)", "owner", owner.String(), "resource", id, "error", err)
		}
		if hadPrev {
			ai.ForgetAzureFoundryCredential(prev)
		}
		// A slot is free: a legacy import waiting for one runs at the next read.
		s.setImportDeferred(owner, false)
	})
	res := AzureConnectionDeleteResult{Status: "ok", DeletedModels: len(deleted)}
	for _, m := range deleted {
		res.ClearedKnowledgeEmbedding = res.ClearedKnowledgeEmbedding || m.IsKnowledgeEmbedding
	}
	return res, nil
}

// ---- Status of a user's resources (single-document call sites) ----

// HasPersonalResources reports whether the caller has at least one Azure AI
// Foundry resource of their own (the provider listing's "configured", the
// /api/keys/has answer): after the legacy import, or — while the import
// could not run — the legacy document itself.
func (s *AzureModelService) HasPersonalResources(ctx context.Context, scope string) (bool, error) {
	owner := s.userOwner(scope)
	s.prepareOwner(ctx, owner, scope)
	conns, err := s.registry.ListConnections(ctx, owner)
	if err != nil {
		return false, err
	}
	for _, c := range conns {
		if !c.Pending {
			return true, nil
		}
	}
	return s.secrets.Has(scope, ai.AzureFoundryProviderID)
}

// OrgHasResources reports whether the organisation holds at least one
// resource (its shared entries exist for its admins and members) — or a
// legacy document whose import is still pending, which will be one.
func (s *AzureModelService) OrgHasResources(ctx context.Context, orgID string) (bool, error) {
	if !s.OrgRegistriesAvailable() || orgID == "" {
		return false, nil
	}
	owner := storageif.AzureOrgOwner(orgID)
	if !s.ensureLegacyImported(ctx, owner, "") {
		return true, nil
	}
	conns, err := s.registry.ListConnections(ctx, owner)
	if err != nil {
		return false, err
	}
	return len(conns) > 0, nil
}
