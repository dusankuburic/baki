package interfaces

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// ---- Azure AI Foundry model registry (Part 9, migration v25) ----
// A user or an organisation owns up to AzureMaxConnections resources ("connections" in code) and AzureMaxModels
// registered models; the credential never lives here (KeyStore / org keystore under "azure-foundry[/<id>]"), only
// metadata. AzureRegistryStore is deliberately NOT part of the StorageBackend composite: three implementations
// (Postgres, the <configDir>/azure-models.json file store, the test fake's in-memory store) run against one
// contract suite (internal/storage/contract). The store enforces the v25 CHECK rules (ValidateAzure*Record),
// per-owner uniqueness, limits and designation uniqueness — and every Postgres query carries an explicit owner
// predicate, because /api/chat/* runs without the request's RLS transaction, where the policies allow all.

// Registry limits, per owner (a user or an organisation).
const (
	AzureMaxConnections = 10
	AzureMaxModels      = 50
)

// AzureDefaultConnectionID is the resource whose credential is the legacy "azure-foundry" key; a new resource never gets this id.
const AzureDefaultConnectionID = "default"

// Model kinds. Claude deployments share the chat namespace for uniqueness (see AzureModelNamespace).
const (
	AzureKindChat      = "chat"
	AzureKindClaude    = "claude"
	AzureKindAgent     = "agent"
	AzureKindEmbedding = "embedding"
)

// Purpose keys. A model's purpose is one preset key, or AzurePurposeCustom with 1–32 chars in PurposeCustom.
const (
	AzurePurposeGeneral       = "general"
	AzurePurposeDocumentation = "documentation"
	AzurePurposeFixes         = "fixes"
	AzurePurposeReview        = "review"
	AzurePurposeRefactoring   = "refactoring"
	AzurePurposeTesting       = "testing"
	AzurePurposeCustom        = "custom"
)

// AzurePurposePresets lists the preset purpose keys in display order.
var AzurePurposePresets = []string{
	AzurePurposeGeneral, AzurePurposeDocumentation, AzurePurposeFixes,
	AzurePurposeReview, AzurePurposeRefactoring, AzurePurposeTesting,
}

// Field bounds, mirrored by the v25 CHECK constraints. Lengths count characters (runes), never bytes.
const (
	AzureNameMaxLen          = 64
	AzureAgentNameMaxLen     = 128
	AzurePurposeCustomMaxLen = 32
	AzureDescriptionMaxLen   = 200
	AzureTestErrorMaxLen     = 500
	AzureContextWindowMin    = 1024
	AzureContextWindowMax    = 2_000_000
	AzureMaxOutputMin        = 256
	AzureMaxOutputMax        = 1_000_000
)

// Registry errors. The not-found errors also match ErrNotFound.
var (
	ErrAzureConnectionNotFound = fmt.Errorf("azure registry: resource %w", ErrNotFound)
	ErrAzureModelNotFound      = fmt.Errorf("azure registry: model %w", ErrNotFound)
	// ErrAzureOwnerNotFound: the Postgres owner row (users / organisations) does not exist.
	ErrAzureOwnerNotFound = fmt.Errorf("azure registry: owner %w", ErrNotFound)

	// Duplicates. Postgres maps SQLSTATE 23505 to these by constraint name — Go's
	// strings.ToLower and Postgres lower() can disagree.
	ErrAzureDuplicateConnectionID   = errors.New("azure registry: a resource with this id already exists")
	ErrAzureDuplicateConnectionName = errors.New("azure registry: a resource with this name already exists")
	ErrAzureDuplicateModelID        = errors.New("azure registry: a model with this id already exists")
	// ErrAzureDuplicateModel: the resource already registers this deployment in the same namespace (claude and chat share one), case-insensitively.
	ErrAzureDuplicateModel = errors.New("azure registry: the resource already has a model with this deployment")
	// ErrAzureDuplicateDesignation: another model of the owner holds the designation.
	ErrAzureDuplicateDesignation = errors.New("azure registry: another model already holds this designation")
	// ErrAzureInvalidDesignation: the model cannot hold it (the knowledge-base embedding must be an embedding model; the org default must not be one; only organisations have one).
	ErrAzureInvalidDesignation = errors.New("azure registry: the model cannot hold this designation")

	ErrAzureTooManyConnections = errors.New("azure registry: resource limit reached")
	ErrAzureTooManyModels      = errors.New("azure registry: model limit reached")

	// ErrAzureRegistryConflict: a compare-and-swap token (ExpectedUpdatedAt) is stale, or — file store — another process rewrote the file between this write's read and rename (409).
	ErrAzureRegistryConflict = errors.New("azure registry: changed by another writer")
	// ErrAzureRegistryReadOnly: the file store's file was written by a newer app version; reads work.
	ErrAzureRegistryReadOnly = errors.New("azure registry: written by a newer version of the app; read-only")
	// ErrAzureRegistryInvalid: the record breaks a storage rule (a v25 CHECK); the service validates first, so this is a backstop.
	ErrAzureRegistryInvalid = errors.New("azure registry: invalid record")
	// ErrAzureRegistryOwnerUnsupported: the owner cannot live in this store (the file store has only the personal owner "").
	ErrAzureRegistryOwnerUnsupported = errors.New("azure registry: owner not supported by this store")
)

// AzureOwnerKind says whether a registry belongs to a user or an org.
type AzureOwnerKind string

const (
	AzureOwnerUser AzureOwnerKind = "user"
	AzureOwnerOrg  AzureOwnerKind = "org"
)

// AzureOwner identifies one registry: a user's personal one or an organisation's curated one (the file store's personal owner is AzureUserOwner("")).
type AzureOwner struct {
	Kind AzureOwnerKind
	ID   string
}

// AzureUserOwner is the personal registry of userID ("" on the desktop).
func AzureUserOwner(userID string) AzureOwner { return AzureOwner{Kind: AzureOwnerUser, ID: userID} }

// AzureOrgOwner is the org-curated registry of orgID.
func AzureOrgOwner(orgID string) AzureOwner { return AzureOwner{Kind: AzureOwnerOrg, ID: orgID} }

// IsOrg reports whether the registry belongs to an organisation.
func (o AzureOwner) IsOrg() bool { return o.Kind == AzureOwnerOrg }

// String is "user:<id>" / "org:<id>", for logs.
func (o AzureOwner) String() string { return string(o.Kind) + ":" + o.ID }

// LockName is the owner lock's name: the Postgres store takes pg_advisory_xact_lock on its hash.
func (o AzureOwner) LockName() string { return "azure-registry:" + o.String() }

// Validate reports an owner no store can hold: an unknown kind, or an organisation without an id.
func (o AzureOwner) Validate() error {
	switch o.Kind {
	case AzureOwnerUser:
		return nil
	case AzureOwnerOrg:
		if o.ID == "" {
			return fmt.Errorf("%w: organisation owner without an id", ErrAzureRegistryOwnerUnsupported)
		}
		return nil
	default:
		return fmt.Errorf("%w: owner kind %q", ErrAzureRegistryOwnerUnsupported, o.Kind)
	}
}

// AzureTestResult is a saved resource's or model's last test.
type AzureTestResult struct {
	OK        bool
	At        time.Time // the store fills in "now" when zero
	LatencyMs int
	Error     string // truncated to AzureTestErrorMaxLen characters by the store
}

// AzureConnectionRecord is one Azure AI Foundry resource of an owner; endpoint, auth mode and secret live in the credential document, not here.
type AzureConnectionRecord struct {
	// ID is AzureDefaultConnectionID or 10 characters of [a-z0-9]; also the credential's key suffix ("azure-foundry/<ID>").
	ID   string
	Name string // 1–64 characters, unique per owner case-insensitively
	// DiscoveryDismissedAt is when the owner dismissed the "found in your project" suggestions.
	DiscoveryDismissedAt *time.Time
	LastTest             *AzureTestResult // nil: never tested

	// Pending and BootID are the file store's two-step create (no transaction spans the registry and the OS keyring):
	// write the row Pending with the process's BootID, save the keyring entry, then FinalizeConnection clears Pending;
	// a Pending row from another BootID was left by an earlier process and the service rolls it back.
	Pending bool
	BootID  string // assigned by the store for a Pending create

	// CreatedAt and UpdatedAt are store-assigned. UpdatedAt is the compare-and-swap token: it moves on UpdateConnection only — never on tests, dismissals or finalising.
	CreatedAt time.Time
	UpdatedAt time.Time
}

// AzureModelRecord is one registered model of an owner, on one resource.
type AzureModelRecord struct {
	ID           string // 10 characters of [a-z0-9]; the picker id is "m/<ID>"
	ConnectionID string
	Kind         string // AzureKindChat, AzureKindClaude, AzureKindAgent or AzureKindEmbedding
	// Deployment is the deployment name ([A-Za-z0-9._-]{1,64}), or for an agent its name (1–128 characters, no control characters).
	Deployment    string
	DisplayName   string // 1–64 characters
	Purpose       string // a preset key or AzurePurposeCustom
	PurposeCustom string // 1–32 characters with AzurePurposeCustom, else ""
	Description   string // at most 200 characters
	// ContextWindow and MaxOutputTokens are optional overrides (0 = unset); always 0 on embeddings.
	ContextWindow   int
	MaxOutputTokens int
	// IsKnowledgeEmbedding marks the owner's knowledge-base embedding model (at most one per owner; embedding models only).
	// IsDefault marks an organisation's "default for members" (at most one per org; never an embedding model; always false
	// for users). CreateModel and ImportModel write both flags as given; only SetDesignation changes them.
	IsKnowledgeEmbedding bool
	IsDefault            bool
	LastTest             *AzureTestResult // nil: never tested
	CreatedAt            time.Time        // assigned by the store
	UpdatedAt            time.Time        // assigned by the store; the compare-and-swap token
}

// AzureConnectionUpdate is UpdateConnection's input.
type AzureConnectionUpdate struct {
	// Name renames the resource; "" keeps the stored name (a touch that only moves UpdatedAt).
	Name string
	// ExpectedUpdatedAt, when non-zero, makes the update a compare-and-swap: a stale token writes nothing and returns ErrAzureRegistryConflict.
	ExpectedUpdatedAt time.Time
}

// AzureModelUpdate replaces a model's editable fields (the caller merges a partial edit onto the stored record first); id, designations, last test and CreatedAt are kept, UpdatedAt moves.
type AzureModelUpdate struct {
	ConnectionID    string
	Kind            string
	Deployment      string
	DisplayName     string
	Purpose         string
	PurposeCustom   string
	Description     string
	ContextWindow   int
	MaxOutputTokens int
	// ExpectedUpdatedAt: see AzureConnectionUpdate.
	ExpectedUpdatedAt time.Time
}

// AzureDesignation names a per-owner, at-most-one-model role.
type AzureDesignation string

const (
	// AzureDesignationKnowledgeEmbedding is the owner's knowledge-base embedding model.
	AzureDesignationKnowledgeEmbedding AzureDesignation = "knowledge_embedding"
	// AzureDesignationOrgDefault is an organisation's "default for members"; users have none.
	AzureDesignationOrgDefault AzureDesignation = "org_default"
)

// AzureRegistryCounts are an owner's row counts, for the limits.
type AzureRegistryCounts struct {
	Connections int
	Models      int
}

// AzureRegistryState is the registry's health, surfaced as registryState.
type AzureRegistryState string

const (
	AzureRegistryOK AzureRegistryState = "ok"
	// AzureRegistryRecovered: the file store found its file corrupt, kept a copy at QuarantinePath and started empty. Sticky for the process.
	AzureRegistryRecovered AzureRegistryState = "recovered"
	// AzureRegistryReadOnly: the file was written by a newer app version; writes fail.
	AzureRegistryReadOnly AzureRegistryState = "read_only"
)

// AzureRegistryStatus is State's result.
type AzureRegistryStatus struct {
	State          AzureRegistryState
	QuarantinePath string // set with AzureRegistryRecovered
}

// AzureRegistryStore persists the Azure AI Foundry registry: resources, models, designations, test stamps and the
// legacy-import marker, per owner. Every method is scoped to one owner; ids of another owner are simply not found.
// Writes run in the request's RLS transaction when ctx carries one (Postgres). Lists return creation order and never nil.
type AzureRegistryStore interface {
	// State reports the registry's health (always ok on Postgres); the file store re-reads its file first, so corruption or a newer-version file is reported before any other call.
	State(ctx context.Context) AzureRegistryStatus
	// PendingWrites reports whether creates must use the Pending protocol: true for the file store, false for transactional Postgres.
	PendingWrites() bool
	// BootID identifies this process's store instance ("" on Postgres).
	BootID() string

	// WithOwnerLock runs fn while holding owner's registry lock, for check-then-act sequences (the limit
	// re-check before a create, the legacy import, designation moves). Postgres: fn runs inside a transaction
	// holding pg_advisory_xact_lock on the owner — the request's RLS transaction when ctx carries one, else one
	// opened here that commits when fn returns nil; RegisterPostCommit hooks run only after that commit. The file
	// and memory stores hold a process mutex (no rollback). Reentrant through fn's ctx. Never run network calls under it.
	WithOwnerLock(ctx context.Context, owner AzureOwner, fn func(ctx context.Context) error) error

	ListConnections(ctx context.Context, owner AzureOwner) ([]AzureConnectionRecord, error)
	// GetConnection returns ErrAzureConnectionNotFound when absent.
	GetConnection(ctx context.Context, owner AzureOwner, id string) (AzureConnectionRecord, error)
	// ConnectionNameTaken reports whether a resource of owner other than exceptID has name under the
	// store's OWN uniqueness rule (Postgres: the unique index on lower(name), whose collation can disagree
	// with Go's). Asked before a credential save, so a rename the store would refuse is refused before anything is written.
	ConnectionNameTaken(ctx context.Context, owner AzureOwner, name, exceptID string) (bool, error)
	// CreateConnection inserts rec and returns the stored record. Errors: the connection duplicates, ErrAzureTooManyConnections, ErrAzureRegistryInvalid.
	CreateConnection(ctx context.Context, owner AzureOwner, rec AzureConnectionRecord) (AzureConnectionRecord, error)
	// ImportConnection is the legacy import's idempotent insert: created is false, with no error, when any uniqueness rule would be broken; ErrAzureTooManyConnections at the limit.
	ImportConnection(ctx context.Context, owner AzureOwner, rec AzureConnectionRecord) (created bool, err error)
	// UpdateConnection renames (or touches) the resource; see AzureConnectionUpdate.
	UpdateConnection(ctx context.Context, owner AzureOwner, id string, u AzureConnectionUpdate) (AzureConnectionRecord, error)
	// FinalizeConnection clears a Pending row's flag (no-op on a non-pending row).
	FinalizeConnection(ctx context.Context, owner AzureOwner, id string) error
	// SetDiscoveryDismissed stamps (keeping an earlier stamp) or clears DiscoveryDismissedAt;
	// UpdatedAt does not move.
	SetDiscoveryDismissed(ctx context.Context, owner AzureOwner, id string, dismissed bool) (AzureConnectionRecord, error)
	// RecordConnectionTest stores the last test (nil clears it); UpdatedAt does not move.
	RecordConnectionTest(ctx context.Context, owner AzureOwner, id string, result *AzureTestResult) error
	// DeleteConnection deletes the resource and, in the same transaction, its models, which it returns; it does not touch the credential (the caller deletes that after the commit).
	DeleteConnection(ctx context.Context, owner AzureOwner, id string) ([]AzureModelRecord, error)

	ListModels(ctx context.Context, owner AzureOwner) ([]AzureModelRecord, error)
	// GetModel returns ErrAzureModelNotFound when absent.
	GetModel(ctx context.Context, owner AzureOwner, id string) (AzureModelRecord, error)
	// CreateModel inserts rec (designation flags included) and returns the stored record. Errors:
	// ErrAzureConnectionNotFound, the duplicate and designation errors, ErrAzureTooManyModels, ErrAzureRegistryInvalid.
	CreateModel(ctx context.Context, owner AzureOwner, rec AzureModelRecord) (AzureModelRecord, error)
	// ImportModel is ImportConnection's counterpart: created is false, with no error, when any uniqueness rule would be broken (the id, the deployment in its namespace, or a designation).
	ImportModel(ctx context.Context, owner AzureOwner, rec AzureModelRecord) (created bool, err error)
	// UpdateModel replaces the editable fields; see AzureModelUpdate. A kind change that would break the model's designation fails with ErrAzureInvalidDesignation.
	UpdateModel(ctx context.Context, owner AzureOwner, id string, u AzureModelUpdate) (AzureModelRecord, error)
	// RecordModelTest stores the last test (nil clears it); UpdatedAt does not move.
	RecordModelTest(ctx context.Context, owner AzureOwner, id string, result *AzureTestResult) error
	// DeleteModel deletes the model and returns it as it was.
	DeleteModel(ctx context.Context, owner AzureOwner, id string) (AzureModelRecord, error)

	// SetDesignation moves designation d to modelID atomically (clearing the previous holder); modelID "" clears it.
	// ErrAzureModelNotFound, ErrAzureInvalidDesignation, ErrAzureDuplicateDesignation (a racing writer — take the owner lock to avoid it).
	SetDesignation(ctx context.Context, owner AzureOwner, d AzureDesignation, modelID string) error

	// Counts returns the owner's row counts (pending rows included).
	Counts(ctx context.Context, owner AzureOwner) (AzureRegistryCounts, error)

	// ImportedAt reads the owner's legacy-import marker; ok is false when the import has not run (the marker survives deleting the default resource, so a deleted import is never resurrected).
	ImportedAt(ctx context.Context, owner AzureOwner) (at time.Time, ok bool, err error)
	// MarkImported sets the marker (keeping an earlier one) and returns it.
	MarkImported(ctx context.Context, owner AzureOwner) (time.Time, error)
}

// ---- GDPR export ----

// AzureConnectionPublicConfig is the secret-free part of a resource's credential document, shown by the data-subject export.
type AzureConnectionPublicConfig struct {
	Endpoint        string `json:"endpoint,omitempty"`
	AuthMode        string `json:"authMode,omitempty"`
	TenantID        string `json:"tenantId,omitempty"`
	ClientID        string `json:"clientId,omitempty"`
	ProjectEndpoint string `json:"projectEndpoint,omitempty"`
	// Unreadable is set when the credential exists but could not be decrypted or parsed.
	Unreadable bool `json:"unreadable,omitempty"`
}

// AzureExportConfigFunc returns the public configuration of userID's resources, keyed by connection id — the
// endpoint/auth/tenant/client ids live inside the encrypted credential document only the service can decrypt,
// so the Postgres backend asks it through this hook (SetAzureExportConfigSource). An error fails the export.
type AzureExportConfigFunc func(ctx context.Context, userID string, connectionIDs []string) (map[string]AzureConnectionPublicConfig, error)

// ExportedAzureRegistry is the data-subject export's registry section: the user's personal resources and models (org registries belong to the org and hold no user ids).
type ExportedAzureRegistry struct {
	Connections []ExportedAzureConnection `json:"connections"`
	Models      []ExportedAzureModel      `json:"models"`
}

// ExportedAzureConnection is one personal resource in the export — its metadata plus the credential's public configuration; never a secret.
type ExportedAzureConnection struct {
	ID                   string                      `json:"id"`
	Name                 string                      `json:"name"`
	Config               AzureConnectionPublicConfig `json:"config"`
	DiscoveryDismissedAt *time.Time                  `json:"discoveryDismissedAt,omitempty"`
	LastTest             *ExportedAzureTest          `json:"lastTest,omitempty"`
	CreatedAt            time.Time                   `json:"createdAt"`
	UpdatedAt            time.Time                   `json:"updatedAt"`
}

// ExportedAzureModel is one personal model in the export (all metadata).
type ExportedAzureModel struct {
	ID                   string             `json:"id"`
	ConnectionID         string             `json:"connectionId"`
	Kind                 string             `json:"kind"`
	Deployment           string             `json:"deployment"`
	DisplayName          string             `json:"displayName"`
	Purpose              string             `json:"purpose"`
	PurposeCustom        string             `json:"purposeCustom,omitempty"`
	Description          string             `json:"description,omitempty"`
	ContextWindow        int                `json:"contextWindow,omitempty"`
	MaxOutputTokens      int                `json:"maxOutputTokens,omitempty"`
	IsKnowledgeEmbedding bool               `json:"isKnowledgeEmbedding"`
	LastTest             *ExportedAzureTest `json:"lastTest,omitempty"`
	CreatedAt            time.Time          `json:"createdAt"`
	UpdatedAt            time.Time          `json:"updatedAt"`
}

// ExportedAzureTest is a last-test stamp in the export.
type ExportedAzureTest struct {
	OK        bool      `json:"ok"`
	At        time.Time `json:"at"`
	LatencyMs int       `json:"latencyMs"`
	Error     string    `json:"error,omitempty"`
}

// NewExportedAzureRegistry maps registry records onto the export shape; config supplies each connection's public configuration (may be nil).
func NewExportedAzureRegistry(conns []AzureConnectionRecord, models []AzureModelRecord, config map[string]AzureConnectionPublicConfig) *ExportedAzureRegistry {
	out := &ExportedAzureRegistry{
		Connections: make([]ExportedAzureConnection, 0, len(conns)),
		Models:      make([]ExportedAzureModel, 0, len(models)),
	}
	for _, c := range conns {
		out.Connections = append(out.Connections, ExportedAzureConnection{
			ID: c.ID, Name: c.Name, Config: config[c.ID],
			DiscoveryDismissedAt: c.DiscoveryDismissedAt, LastTest: exportedAzureTest(c.LastTest),
			CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt,
		})
	}
	for _, m := range models {
		out.Models = append(out.Models, ExportedAzureModel{
			ID: m.ID, ConnectionID: m.ConnectionID, Kind: m.Kind, Deployment: m.Deployment,
			DisplayName: m.DisplayName, Purpose: m.Purpose, PurposeCustom: m.PurposeCustom,
			Description: m.Description, ContextWindow: m.ContextWindow, MaxOutputTokens: m.MaxOutputTokens,
			IsKnowledgeEmbedding: m.IsKnowledgeEmbedding, LastTest: exportedAzureTest(m.LastTest),
			CreatedAt: m.CreatedAt, UpdatedAt: m.UpdatedAt,
		})
	}
	return out
}

func exportedAzureTest(r *AzureTestResult) *ExportedAzureTest {
	if r == nil {
		return nil
	}
	return &ExportedAzureTest{OK: r.OK, At: r.At, LatencyMs: r.LatencyMs, Error: r.Error}
}

// ---- Validation (the v25 CHECK constraints, for every implementation) ----

var (
	azureGeneratedID  = regexp.MustCompile(`^[a-z0-9]{10}$`)
	azureDeploymentRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
)

// ValidAzureConnectionID reports whether id can name a resource: AzureDefaultConnectionID or 10 chars of [a-z0-9].
func ValidAzureConnectionID(id string) bool {
	return id == AzureDefaultConnectionID || azureGeneratedID.MatchString(id)
}

// ValidAzureModelID reports whether id can name a model: 10 characters of [a-z0-9] ("default" is a resource id only).
func ValidAzureModelID(id string) bool { return azureGeneratedID.MatchString(id) }

// ValidAzureKind reports whether kind is one of the four model kinds.
func ValidAzureKind(kind string) bool {
	switch kind {
	case AzureKindChat, AzureKindClaude, AzureKindAgent, AzureKindEmbedding:
		return true
	}
	return false
}

// AzureModelNamespace is the uniqueness namespace of a kind: claude deployments share chat's.
func AzureModelNamespace(kind string) string {
	if kind == AzureKindClaude {
		return AzureKindChat
	}
	return kind
}

// ValidAzurePurpose reports whether purpose / custom form a valid pair: a preset key with no custom text, or AzurePurposeCustom with 1–32 characters.
func ValidAzurePurpose(purpose, custom string) bool {
	if purpose == AzurePurposeCustom {
		n := utf8.RuneCountInString(custom)
		return n >= 1 && n <= AzurePurposeCustomMaxLen
	}
	for _, p := range AzurePurposePresets {
		if purpose == p {
			return custom == ""
		}
	}
	return false
}

// ValidateAzureDeployment checks a deployment name for kind: agents 1–128 characters with no control characters, every other kind [A-Za-z0-9._-]{1,64}.
func ValidateAzureDeployment(kind, name string) error {
	if kind == AzureKindAgent {
		n := utf8.RuneCountInString(name)
		if n < 1 || n > AzureAgentNameMaxLen || !utf8.ValidString(name) || strings.ContainsFunc(name, isASCIIControl) {
			return fmt.Errorf("%w: agent name must be 1-%d characters without control characters", ErrAzureRegistryInvalid, AzureAgentNameMaxLen)
		}
		return nil
	}
	if !azureDeploymentRe.MatchString(name) {
		return fmt.Errorf("%w: deployment name must match [A-Za-z0-9._-]{1,64}", ErrAzureRegistryInvalid)
	}
	return nil
}

// isASCIIControl matches the characters the v25 agent-name CHECK refuses (C0 controls and DEL).
func isASCIIControl(r rune) bool { return r < 0x20 || r == 0x7f }

// TruncateAzureTestError shortens a test error to AzureTestErrorMaxLen characters (never splitting one).
func TruncateAzureTestError(s string) string {
	if utf8.RuneCountInString(s) <= AzureTestErrorMaxLen {
		return s
	}
	runes := []rune(s)
	return string(runes[:AzureTestErrorMaxLen])
}

// ValidateAzureConnectionRecord checks the storage rules for a resource row (id, name, last test).
func ValidateAzureConnectionRecord(rec AzureConnectionRecord) error {
	if !ValidAzureConnectionID(rec.ID) {
		return fmt.Errorf("%w: resource id %q", ErrAzureRegistryInvalid, rec.ID)
	}
	if err := validAzureText("name", rec.Name, 1, AzureNameMaxLen); err != nil {
		return err
	}
	return validAzureTest(rec.LastTest)
}

// ValidateAzureModelRecord checks the storage rules for a model row of owner, designations included.
func ValidateAzureModelRecord(owner AzureOwner, rec AzureModelRecord) error {
	if !ValidAzureModelID(rec.ID) {
		return fmt.Errorf("%w: model id %q", ErrAzureRegistryInvalid, rec.ID)
	}
	if err := validateAzureModelFields(rec.ConnectionID, rec.Kind, rec.Deployment, rec.DisplayName, rec.Purpose, rec.PurposeCustom, rec.Description, rec.ContextWindow, rec.MaxOutputTokens); err != nil {
		return err
	}
	if rec.IsKnowledgeEmbedding && rec.Kind != AzureKindEmbedding {
		return fmt.Errorf("%w: only an embedding model can be the knowledge-base model", ErrAzureInvalidDesignation)
	}
	if rec.IsDefault && (!owner.IsOrg() || rec.Kind == AzureKindEmbedding) {
		return fmt.Errorf("%w: only an organisation's chat, claude or agent model can be its default", ErrAzureInvalidDesignation)
	}
	return validAzureTest(rec.LastTest)
}

// ValidateAzureModelUpdate checks an update's fields (designations are checked against the stored row by the store).
func ValidateAzureModelUpdate(u AzureModelUpdate) error {
	return validateAzureModelFields(u.ConnectionID, u.Kind, u.Deployment, u.DisplayName, u.Purpose, u.PurposeCustom, u.Description, u.ContextWindow, u.MaxOutputTokens)
}

func validateAzureModelFields(connectionID, kind, deployment, displayName, purpose, purposeCustom, description string, contextWindow, maxOutput int) error {
	if !ValidAzureConnectionID(connectionID) {
		return fmt.Errorf("%w: resource id %q", ErrAzureRegistryInvalid, connectionID)
	}
	if !ValidAzureKind(kind) {
		return fmt.Errorf("%w: kind %q", ErrAzureRegistryInvalid, kind)
	}
	if err := ValidateAzureDeployment(kind, deployment); err != nil {
		return err
	}
	if err := validAzureText("display name", displayName, 1, AzureNameMaxLen); err != nil {
		return err
	}
	if !utf8.ValidString(purposeCustom) || strings.ContainsRune(purposeCustom, 0) || !ValidAzurePurpose(purpose, purposeCustom) {
		return fmt.Errorf("%w: purpose %q", ErrAzureRegistryInvalid, purpose)
	}
	if err := validAzureText("description", description, 0, AzureDescriptionMaxLen); err != nil {
		return err
	}
	if kind == AzureKindEmbedding {
		if contextWindow != 0 || maxOutput != 0 {
			return fmt.Errorf("%w: embedding models have no context window or max output", ErrAzureRegistryInvalid)
		}
		return nil
	}
	if contextWindow != 0 && (contextWindow < AzureContextWindowMin || contextWindow > AzureContextWindowMax) {
		return fmt.Errorf("%w: context window %d", ErrAzureRegistryInvalid, contextWindow)
	}
	if maxOutput != 0 && (maxOutput < AzureMaxOutputMin || maxOutput > AzureMaxOutputMax) {
		return fmt.Errorf("%w: max output tokens %d", ErrAzureRegistryInvalid, maxOutput)
	}
	return nil
}

// validAzureText checks a text column: valid UTF-8 without NUL (Postgres text can hold neither) and min–max characters.
func validAzureText(field, s string, minLen, maxLen int) error {
	n := utf8.RuneCountInString(s)
	if !utf8.ValidString(s) || strings.ContainsRune(s, 0) || n < minLen || n > maxLen {
		return fmt.Errorf("%w: %s must be %d-%d characters of valid text", ErrAzureRegistryInvalid, field, minLen, maxLen)
	}
	return nil
}

func validAzureTest(r *AzureTestResult) error {
	if r == nil {
		return nil
	}
	if r.LatencyMs < 0 {
		return fmt.Errorf("%w: negative test latency", ErrAzureRegistryInvalid)
	}
	return validAzureText("test error", r.Error, 0, AzureTestErrorMaxLen)
}
