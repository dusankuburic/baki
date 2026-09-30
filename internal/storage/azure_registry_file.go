package storage

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"pad-analyzer/internal/storage/atomicfile"
	"pad-analyzer/internal/storage/interfaces"
	"pad-core/logger"
)

// AzureRegistryFileName is the file store's file inside the config dir. It
// is never settings.json: the registry has its own file, format version and
// revision counter, so neither can clobber the other.
const AzureRegistryFileName = "azure-models.json"

// azureRegistryFileVersion is the file format this binary writes. A file
// with a higher version was written by a newer app: it is read (leniently)
// but never rewritten — the store reports read_only.
const azureRegistryFileVersion = 1

// FileAzureRegistry is the AzureRegistryStore of the desktop app and of a
// database backend with auth off: one personal owner (AzureUserOwner("")),
// persisted in <configDir>/azure-models.json.
//
//   - Every read-modify-write holds one process mutex and re-reads the file
//     inside it, so writers in this process never lose each other's work.
//   - Writes go to a unique temp file (mode 0600) renamed over the target,
//     with the filesystem backend's bounded retry over a Windows reader's
//     transient sharing conflict.
//   - The file carries a revision counter, compared again right before the
//     rename: a second process sharing the directory (a standalone backend
//     next to the desktop app — Tauri's single-instance plugin does not
//     stop it) that wrote meanwhile turns this write into
//     ErrAzureRegistryConflict instead of silently discarding its write.
//     The compare and the rename are not one atomic step, so two processes
//     committing in the same instant can still race; the counter closes the
//     window a read-modify-write leaves open, which is where the time goes.
//   - A corrupt file is renamed aside (the copy is kept, its path reported
//     by State) and the registry starts empty: registryState recovered.
//   - A file from a newer format version is read-only: registryState
//     read_only, writes fail with ErrAzureRegistryReadOnly.
//
// There is no transaction spanning this file and the OS keyring, so a
// resource is created Pending with this process's BootID (see
// AzureConnectionRecord.Pending); PendingWrites reports true.
type FileAzureRegistry struct {
	path   string
	bootID string

	fileMu  sync.Mutex // every load and read-modify-write of the file
	ownerMu sync.Mutex // WithOwnerLock (separate: fn calls the other methods)

	stateMu        sync.Mutex
	readOnly       bool   // the last load saw a newer format version
	quarantinePath string // set once a corrupt file was renamed aside

	// beforeCommit, when set (tests only), runs after the temp file is
	// written and before the revision re-check and the rename.
	beforeCommit func()
}

var _ interfaces.AzureRegistryStore = (*FileAzureRegistry)(nil)

// NewFileAzureRegistry opens the registry file in configDir (created on
// the first write). It loads the file once, so a corrupt file is
// quarantined — and a newer one detected — at startup rather than on the
// first request; an I/O error doing so is returned.
func NewFileAzureRegistry(configDir string) (*FileAzureRegistry, error) {
	if configDir == "" {
		return nil, errors.New("azure registry: empty config directory")
	}
	s := &FileAzureRegistry{
		path:   filepath.Join(configDir, AzureRegistryFileName),
		bootID: newAzureBootID(),
	}
	s.fileMu.Lock()
	_, _, err := s.loadLocked()
	s.fileMu.Unlock()
	if err != nil {
		return nil, err
	}
	return s, nil
}

// newAzureBootID returns a random per-process id for pending rows.
func newAzureBootID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(b[:])
}

// Path is the registry file's path (shown with a recovered state).
func (s *FileAzureRegistry) Path() string { return s.path }

// ---- file format ----

type azureRegistryFile struct {
	Version     int                   `json:"version"`
	Revision    int64                 `json:"revision"`
	ImportedAt  *time.Time            `json:"importedAt,omitempty"`
	Connections []azureFileConnection `json:"connections"`
	Models      []azureFileModel      `json:"models"`
}

type azureFileTest struct {
	OK        bool      `json:"ok"`
	At        time.Time `json:"at"`
	LatencyMs int       `json:"latencyMs"`
	Error     string    `json:"error,omitempty"`
}

type azureFileConnection struct {
	ID                   string         `json:"id"`
	Name                 string         `json:"name"`
	DiscoveryDismissedAt *time.Time     `json:"discoveryDismissedAt,omitempty"`
	LastTest             *azureFileTest `json:"lastTest,omitempty"`
	Pending              bool           `json:"pending,omitempty"`
	BootID               string         `json:"bootId,omitempty"`
	CreatedAt            time.Time      `json:"createdAt"`
	UpdatedAt            time.Time      `json:"updatedAt"`
}

type azureFileModel struct {
	ID                   string         `json:"id"`
	ConnectionID         string         `json:"connectionId"`
	Kind                 string         `json:"kind"`
	Deployment           string         `json:"deployment"`
	DisplayName          string         `json:"displayName"`
	Purpose              string         `json:"purpose"`
	PurposeCustom        string         `json:"purposeCustom,omitempty"`
	Description          string         `json:"description,omitempty"`
	ContextWindow        int            `json:"contextWindow,omitempty"`
	MaxOutputTokens      int            `json:"maxOutputTokens,omitempty"`
	IsKnowledgeEmbedding bool           `json:"isKnowledgeEmbedding,omitempty"`
	LastTest             *azureFileTest `json:"lastTest,omitempty"`
	CreatedAt            time.Time      `json:"createdAt"`
	UpdatedAt            time.Time      `json:"updatedAt"`
}

func toFileTest(r *interfaces.AzureTestResult) *azureFileTest {
	if r == nil {
		return nil
	}
	return &azureFileTest{OK: r.OK, At: r.At, LatencyMs: r.LatencyMs, Error: r.Error}
}

func fromFileTest(r *azureFileTest) *interfaces.AzureTestResult {
	if r == nil {
		return nil
	}
	return &interfaces.AzureTestResult{OK: r.OK, At: r.At.UTC(), LatencyMs: r.LatencyMs, Error: r.Error}
}

func utcPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

func (f *azureRegistryFile) toData() *azureRegistryData {
	d := &azureRegistryData{importedAt: utcPtr(f.ImportedAt)}
	for _, c := range f.Connections {
		d.connections = append(d.connections, interfaces.AzureConnectionRecord{
			ID: c.ID, Name: c.Name, DiscoveryDismissedAt: utcPtr(c.DiscoveryDismissedAt),
			LastTest: fromFileTest(c.LastTest), Pending: c.Pending, BootID: c.BootID,
			CreatedAt: c.CreatedAt.UTC(), UpdatedAt: c.UpdatedAt.UTC(),
		})
	}
	for _, m := range f.Models {
		d.models = append(d.models, interfaces.AzureModelRecord{
			ID: m.ID, ConnectionID: m.ConnectionID, Kind: m.Kind, Deployment: m.Deployment,
			DisplayName: m.DisplayName, Purpose: m.Purpose, PurposeCustom: m.PurposeCustom,
			Description: m.Description, ContextWindow: m.ContextWindow, MaxOutputTokens: m.MaxOutputTokens,
			IsKnowledgeEmbedding: m.IsKnowledgeEmbedding, LastTest: fromFileTest(m.LastTest),
			CreatedAt: m.CreatedAt.UTC(), UpdatedAt: m.UpdatedAt.UTC(),
		})
	}
	return d
}

func azureFileFromData(d *azureRegistryData, revision int64) *azureRegistryFile {
	f := &azureRegistryFile{
		Version: azureRegistryFileVersion, Revision: revision, ImportedAt: d.importedAt,
		Connections: []azureFileConnection{}, Models: []azureFileModel{},
	}
	for _, c := range d.listConnections() {
		f.Connections = append(f.Connections, azureFileConnection{
			ID: c.ID, Name: c.Name, DiscoveryDismissedAt: c.DiscoveryDismissedAt,
			LastTest: toFileTest(c.LastTest), Pending: c.Pending, BootID: c.BootID,
			CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt,
		})
	}
	for _, m := range d.listModels() {
		f.Models = append(f.Models, azureFileModel{
			ID: m.ID, ConnectionID: m.ConnectionID, Kind: m.Kind, Deployment: m.Deployment,
			DisplayName: m.DisplayName, Purpose: m.Purpose, PurposeCustom: m.PurposeCustom,
			Description: m.Description, ContextWindow: m.ContextWindow, MaxOutputTokens: m.MaxOutputTokens,
			IsKnowledgeEmbedding: m.IsKnowledgeEmbedding, LastTest: toFileTest(m.LastTest),
			CreatedAt: m.CreatedAt, UpdatedAt: m.UpdatedAt,
		})
	}
	return f
}

// fileOwner is the file store's only owner.
var fileOwner = interfaces.AzureUserOwner("")

func (s *FileAzureRegistry) checkOwner(owner interfaces.AzureOwner) error {
	if owner != fileOwner {
		return fmt.Errorf("%w: the file store holds only the personal owner \"\", got %s", interfaces.ErrAzureRegistryOwnerUnsupported, owner)
	}
	return nil
}

// errAzureFileChanged: the file changed between reading it and quarantining
// it (another process rewrote it) — load again.
var errAzureFileChanged = errors.New("azure registry file changed while loading")

// loadLocked reads and parses the file (fileMu held). A missing file is an
// empty registry at revision 0; a corrupt one is quarantined and also reads
// as empty; a newer-version one is parsed leniently and flagged read-only.
func (s *FileAzureRegistry) loadLocked() (d *azureRegistryData, revision int64, err error) {
	for range 3 {
		d, revision, err = s.loadOnceLocked()
		if !errors.Is(err, errAzureFileChanged) {
			return d, revision, err
		}
	}
	return nil, 0, fmt.Errorf("azure registry: %s keeps changing while being read", s.path)
}

func (s *FileAzureRegistry) loadOnceLocked() (*azureRegistryData, int64, error) {
	data, err := atomicfile.ReadFileTolerant(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		s.setReadOnly(false)
		return &azureRegistryData{}, 0, nil
	}
	if err != nil {
		return nil, 0, fmt.Errorf("azure registry: read %s: %w", s.path, err)
	}
	var head struct {
		Version  int   `json:"version"`
		Revision int64 `json:"revision"`
	}
	if err := json.Unmarshal(data, &head); err != nil {
		return s.quarantineLocked(data, err)
	}
	if head.Version > azureRegistryFileVersion {
		// A newer app wrote it. Show what this version understands, but
		// never rewrite it (and never quarantine it: that would hide the
		// newer app's data from the newer app).
		s.setReadOnly(true)
		var doc azureRegistryFile
		if json.Unmarshal(data, &doc) != nil {
			return &azureRegistryData{}, head.Revision, nil
		}
		return doc.toData(), head.Revision, nil
	}
	if head.Version < 1 {
		return s.quarantineLocked(data, fmt.Errorf("format version %d", head.Version))
	}
	var doc azureRegistryFile
	if err := json.Unmarshal(data, &doc); err != nil {
		return s.quarantineLocked(data, err)
	}
	d := doc.toData()
	if err := d.checkInvariants(fileOwner); err != nil {
		return s.quarantineLocked(data, err)
	}
	s.setReadOnly(false)
	return d, doc.Revision, nil
}

func (s *FileAzureRegistry) setReadOnly(v bool) {
	s.stateMu.Lock()
	s.readOnly = v
	s.stateMu.Unlock()
}

// quarantineLocked renames the corrupt file aside — keeping the copy for
// the user or support — and reports an empty registry at revision 0, so
// the next write starts a fresh file. The file is re-read first: if another
// process replaced it meanwhile, that newer content is loaded instead.
func (s *FileAzureRegistry) quarantineLocked(data []byte, cause error) (*azureRegistryData, int64, error) {
	current, err := atomicfile.ReadFileTolerant(s.path)
	if err == nil && !bytes.Equal(current, data) || errors.Is(err, fs.ErrNotExist) {
		return nil, 0, errAzureFileChanged
	}
	qpath := s.path + ".corrupt-" + time.Now().UTC().Format("20060102T150405.000000000")
	if err := atomicfile.RenameOverOpenReader(s.path, qpath); err != nil {
		logger.Error("azure registry: failed to quarantine corrupt file",
			"path", s.path, "rename_error", err, "parse_error", cause)
		return nil, 0, fmt.Errorf("azure registry: %s is corrupt and could not be moved aside: %w", s.path, err)
	}
	logger.Error("azure registry: corrupt file quarantined, starting empty",
		"path", s.path, "backup", qpath, "parse_error", cause)
	s.stateMu.Lock()
	s.quarantinePath = qpath
	s.readOnly = false
	s.stateMu.Unlock()
	return &azureRegistryData{}, 0, nil
}

// peekRevision reads the revision currently on disk: 0 when the file is
// absent; ok is false when it cannot be read as a file this version may
// overwrite (unparseable, or from a newer version).
func (s *FileAzureRegistry) peekRevision() (revision int64, ok bool) {
	data, err := atomicfile.ReadFileTolerant(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, true
	}
	if err != nil {
		return 0, false
	}
	var head struct {
		Version  int   `json:"version"`
		Revision int64 `json:"revision"`
	}
	if json.Unmarshal(data, &head) != nil || head.Version > azureRegistryFileVersion {
		return 0, false
	}
	return head.Revision, true
}

// commitLocked writes d as revision base+1 (fileMu held): temp file, the
// revision re-check, then the rename.
func (s *FileAzureRegistry) commitLocked(d *azureRegistryData, base int64) error {
	data, err := json.MarshalIndent(azureFileFromData(d, base+1), "", "  ")
	if err != nil {
		return fmt.Errorf("azure registry: encode: %w", err)
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("azure registry: create %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".azure-models-tmp-*")
	if err != nil {
		return fmt.Errorf("azure registry: temp file: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) } // best-effort; never masks the real error
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("azure registry: temp file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("azure registry: write: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("azure registry: write: %w", err)
	}
	if s.beforeCommit != nil {
		s.beforeCommit()
	}
	if current, ok := s.peekRevision(); !ok || current != base {
		cleanup()
		return interfaces.ErrAzureRegistryConflict
	}
	if err := atomicfile.RenameOverOpenReader(tmpName, s.path); err != nil {
		logger.Error("azure registry: atomic rename failed", "tmp", tmpName, "target", s.path, "error", err)
		cleanup()
		return fmt.Errorf("azure registry: replace %s: %w", s.path, err)
	}
	return nil
}

// view runs fn on the current file contents.
func (s *FileAzureRegistry) view(owner interfaces.AzureOwner, fn func(d *azureRegistryData) error) error {
	if err := s.checkOwner(owner); err != nil {
		return err
	}
	s.fileMu.Lock()
	defer s.fileMu.Unlock()
	d, _, err := s.loadLocked()
	if err != nil {
		return err
	}
	return fn(d)
}

// update runs a read-modify-write: fn mutates the freshly loaded data and
// reports whether it changed anything; only a change is written.
func (s *FileAzureRegistry) update(owner interfaces.AzureOwner, fn func(d *azureRegistryData) (changed bool, err error)) error {
	if err := s.checkOwner(owner); err != nil {
		return err
	}
	s.fileMu.Lock()
	defer s.fileMu.Unlock()
	d, revision, err := s.loadLocked()
	if err != nil {
		return err
	}
	s.stateMu.Lock()
	readOnly := s.readOnly
	s.stateMu.Unlock()
	if readOnly {
		return interfaces.ErrAzureRegistryReadOnly
	}
	changed, err := fn(d)
	if err != nil || !changed {
		return err
	}
	return s.commitLocked(d, revision)
}

// State re-reads the file, then reports read_only (a newer version's file),
// recovered (a corrupt file was quarantined during this process's life) or
// ok.
func (s *FileAzureRegistry) State(context.Context) interfaces.AzureRegistryStatus {
	s.fileMu.Lock()
	if _, _, err := s.loadLocked(); err != nil {
		logger.Warn("azure registry: state check could not read the file", "path", s.path, "error", err)
	}
	s.fileMu.Unlock()
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	switch {
	case s.readOnly:
		return interfaces.AzureRegistryStatus{State: interfaces.AzureRegistryReadOnly}
	case s.quarantinePath != "":
		return interfaces.AzureRegistryStatus{State: interfaces.AzureRegistryRecovered, QuarantinePath: s.quarantinePath}
	default:
		return interfaces.AzureRegistryStatus{State: interfaces.AzureRegistryOK}
	}
}

// PendingWrites is true: creates use the Pending protocol.
func (s *FileAzureRegistry) PendingWrites() bool { return true }

// BootID identifies this store instance (one per process).
func (s *FileAzureRegistry) BootID() string { return s.bootID }

type fileAzureLockKey struct{ store *FileAzureRegistry }

// WithOwnerLock holds the store's owner mutex while fn runs (reentrant
// through fn's ctx). It serialises check-then-act sequences within this
// process; another process is caught by the revision check instead.
func (s *FileAzureRegistry) WithOwnerLock(ctx context.Context, owner interfaces.AzureOwner, fn func(ctx context.Context) error) error {
	if err := s.checkOwner(owner); err != nil {
		return err
	}
	key := fileAzureLockKey{s}
	if ctx.Value(key) != nil {
		return fn(ctx)
	}
	s.ownerMu.Lock()
	defer s.ownerMu.Unlock()
	return fn(context.WithValue(ctx, key, true))
}

func (s *FileAzureRegistry) ListConnections(_ context.Context, owner interfaces.AzureOwner) (out []interfaces.AzureConnectionRecord, err error) {
	err = s.view(owner, func(d *azureRegistryData) error {
		out = d.listConnections()
		return nil
	})
	return out, err
}

func (s *FileAzureRegistry) GetConnection(_ context.Context, owner interfaces.AzureOwner, id string) (out interfaces.AzureConnectionRecord, err error) {
	err = s.view(owner, func(d *azureRegistryData) (e error) {
		out, e = d.getConnection(id)
		return e
	})
	return out, err
}

func (s *FileAzureRegistry) ConnectionNameTaken(_ context.Context, owner interfaces.AzureOwner, name, exceptID string) (taken bool, err error) {
	err = s.view(owner, func(d *azureRegistryData) error {
		taken = d.connectionNameTaken(name, exceptID)
		return nil
	})
	return taken, err
}

func (s *FileAzureRegistry) CreateConnection(_ context.Context, owner interfaces.AzureOwner, rec interfaces.AzureConnectionRecord) (out interfaces.AzureConnectionRecord, err error) {
	err = s.update(owner, func(d *azureRegistryData) (bool, error) {
		var e error
		out, e = d.createConnection(rec, true, s.bootID)
		return e == nil, e
	})
	return out, err
}

func (s *FileAzureRegistry) ImportConnection(_ context.Context, owner interfaces.AzureOwner, rec interfaces.AzureConnectionRecord) (created bool, err error) {
	err = s.update(owner, func(d *azureRegistryData) (bool, error) {
		var e error
		created, e = d.importConnection(rec)
		return created, e
	})
	return created, err
}

func (s *FileAzureRegistry) UpdateConnection(_ context.Context, owner interfaces.AzureOwner, id string, u interfaces.AzureConnectionUpdate) (out interfaces.AzureConnectionRecord, err error) {
	err = s.update(owner, func(d *azureRegistryData) (bool, error) {
		var e error
		out, e = d.updateConnection(id, u)
		return e == nil, e
	})
	return out, err
}

func (s *FileAzureRegistry) FinalizeConnection(_ context.Context, owner interfaces.AzureOwner, id string) error {
	return s.update(owner, func(d *azureRegistryData) (bool, error) {
		return d.finalizeConnection(id)
	})
}

func (s *FileAzureRegistry) SetDiscoveryDismissed(_ context.Context, owner interfaces.AzureOwner, id string, dismissed bool) (out interfaces.AzureConnectionRecord, err error) {
	err = s.update(owner, func(d *azureRegistryData) (bool, error) {
		var (
			changed bool
			e       error
		)
		out, changed, e = d.setDiscoveryDismissed(id, dismissed)
		return changed, e
	})
	return out, err
}

func (s *FileAzureRegistry) RecordConnectionTest(_ context.Context, owner interfaces.AzureOwner, id string, result *interfaces.AzureTestResult) error {
	return s.update(owner, func(d *azureRegistryData) (bool, error) {
		return true, d.recordConnectionTest(id, result)
	})
}

func (s *FileAzureRegistry) DeleteConnection(_ context.Context, owner interfaces.AzureOwner, id string) (out []interfaces.AzureModelRecord, err error) {
	err = s.update(owner, func(d *azureRegistryData) (bool, error) {
		var e error
		out, e = d.deleteConnection(id)
		return e == nil, e
	})
	return out, err
}

func (s *FileAzureRegistry) ListModels(_ context.Context, owner interfaces.AzureOwner) (out []interfaces.AzureModelRecord, err error) {
	err = s.view(owner, func(d *azureRegistryData) error {
		out = d.listModels()
		return nil
	})
	return out, err
}

func (s *FileAzureRegistry) GetModel(_ context.Context, owner interfaces.AzureOwner, id string) (out interfaces.AzureModelRecord, err error) {
	err = s.view(owner, func(d *azureRegistryData) (e error) {
		out, e = d.getModel(id)
		return e
	})
	return out, err
}

func (s *FileAzureRegistry) CreateModel(_ context.Context, owner interfaces.AzureOwner, rec interfaces.AzureModelRecord) (out interfaces.AzureModelRecord, err error) {
	err = s.update(owner, func(d *azureRegistryData) (bool, error) {
		var e error
		out, e = d.createModel(owner, rec)
		return e == nil, e
	})
	return out, err
}

func (s *FileAzureRegistry) ImportModel(_ context.Context, owner interfaces.AzureOwner, rec interfaces.AzureModelRecord) (created bool, err error) {
	err = s.update(owner, func(d *azureRegistryData) (bool, error) {
		var e error
		created, e = d.importModel(owner, rec)
		return created, e
	})
	return created, err
}

func (s *FileAzureRegistry) UpdateModel(_ context.Context, owner interfaces.AzureOwner, id string, u interfaces.AzureModelUpdate) (out interfaces.AzureModelRecord, err error) {
	err = s.update(owner, func(d *azureRegistryData) (bool, error) {
		var e error
		out, e = d.updateModel(id, u)
		return e == nil, e
	})
	return out, err
}

func (s *FileAzureRegistry) RecordModelTest(_ context.Context, owner interfaces.AzureOwner, id string, result *interfaces.AzureTestResult) error {
	return s.update(owner, func(d *azureRegistryData) (bool, error) {
		return true, d.recordModelTest(id, result)
	})
}

func (s *FileAzureRegistry) DeleteModel(_ context.Context, owner interfaces.AzureOwner, id string) (out interfaces.AzureModelRecord, err error) {
	err = s.update(owner, func(d *azureRegistryData) (bool, error) {
		var e error
		out, e = d.deleteModel(id)
		return e == nil, e
	})
	return out, err
}

func (s *FileAzureRegistry) SetDesignation(_ context.Context, owner interfaces.AzureOwner, desig interfaces.AzureDesignation, modelID string) error {
	return s.update(owner, func(d *azureRegistryData) (bool, error) {
		return true, d.setDesignation(owner, desig, modelID)
	})
}

func (s *FileAzureRegistry) Counts(_ context.Context, owner interfaces.AzureOwner) (out interfaces.AzureRegistryCounts, err error) {
	err = s.view(owner, func(d *azureRegistryData) error {
		out = d.counts()
		return nil
	})
	return out, err
}

func (s *FileAzureRegistry) ImportedAt(_ context.Context, owner interfaces.AzureOwner) (at time.Time, ok bool, err error) {
	err = s.view(owner, func(d *azureRegistryData) error {
		if d.importedAt != nil {
			at, ok = *d.importedAt, true
		}
		return nil
	})
	return at, ok, err
}

func (s *FileAzureRegistry) MarkImported(_ context.Context, owner interfaces.AzureOwner) (at time.Time, err error) {
	err = s.update(owner, func(d *azureRegistryData) (bool, error) {
		already := d.importedAt != nil
		at = d.markImported()
		return !already, nil
	})
	return at, err
}
