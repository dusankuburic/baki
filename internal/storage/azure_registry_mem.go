package storage

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"pad-analyzer/internal/storage/interfaces"
)

// azureRegistryData is one owner's Azure AI Foundry registry held in memory:
// the engine behind the file store and MemoryAzureRegistry. It applies the v25
// schema's rules in Go in Postgres's order — validation (CHECKs) → limit →
// uniqueness → existence — so the three implementations answer alike (the
// contract suite pins it). Records go in and out as copies.
type azureRegistryData struct {
	connections []interfaces.AzureConnectionRecord
	models      []interfaces.AzureModelRecord
	importedAt  *time.Time
}

// azureNow is the store clock at the database's precision (microseconds),
// so a compare-and-swap token round-trips identically on every store.
func azureNow() time.Time { return time.Now().UTC().Truncate(time.Microsecond) }

// azureBump is the next UpdatedAt after prev: now, but always strictly later
// (Postgres: GREATEST(clock_timestamp(), updated_at + 1µs)), so every update
// hands out a fresh CAS token.
func azureBump(prev time.Time) time.Time {
	now := azureNow()
	if !now.After(prev) {
		now = prev.Add(time.Microsecond)
	}
	return now
}

// azureTokenMatches compares a caller's compare-and-swap token with the stored UpdatedAt at microsecond
// precision, as Postgres does (pgx drops the nanoseconds when it encodes the parameter).
func azureTokenMatches(expected, stored time.Time) bool {
	return expected.Truncate(time.Microsecond).Equal(stored)
}

func cloneAzureTest(r *interfaces.AzureTestResult) *interfaces.AzureTestResult {
	if r == nil {
		return nil
	}
	c := *r
	return &c
}

func cloneTime(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	c := *t
	return &c
}

func cloneAzureConnection(c interfaces.AzureConnectionRecord) interfaces.AzureConnectionRecord {
	c.DiscoveryDismissedAt = cloneTime(c.DiscoveryDismissedAt)
	c.LastTest = cloneAzureTest(c.LastTest)
	return c
}

func cloneAzureModel(m interfaces.AzureModelRecord) interfaces.AzureModelRecord {
	m.LastTest = cloneAzureTest(m.LastTest)
	return m
}

// normalizeAzureTest prepares a test stamp for storage: the error is
// truncated to its column's length and a zero time becomes now.
func normalizeAzureTest(r *interfaces.AzureTestResult) (*interfaces.AzureTestResult, error) {
	if r == nil {
		return nil, nil
	}
	c := *r
	c.Error = interfaces.TruncateAzureTestError(c.Error)
	if c.At.IsZero() {
		c.At = azureNow()
	} else {
		c.At = c.At.UTC().Truncate(time.Microsecond)
	}
	if c.LatencyMs < 0 {
		return nil, fmt.Errorf("%w: negative test latency", interfaces.ErrAzureRegistryInvalid)
	}
	return &c, nil
}

func (d *azureRegistryData) connIndex(id string) int {
	for i := range d.connections {
		if d.connections[i].ID == id {
			return i
		}
	}
	return -1
}

func (d *azureRegistryData) modelIndex(id string) int {
	for i := range d.models {
		if d.models[i].ID == id {
			return i
		}
	}
	return -1
}

// listConnections returns copies in creation order (CreatedAt, then ID).
func (d *azureRegistryData) listConnections() []interfaces.AzureConnectionRecord {
	out := make([]interfaces.AzureConnectionRecord, 0, len(d.connections))
	for _, c := range d.connections {
		out = append(out, cloneAzureConnection(c))
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// listModels returns copies in creation order (CreatedAt, then ID).
func (d *azureRegistryData) listModels() []interfaces.AzureModelRecord {
	out := make([]interfaces.AzureModelRecord, 0, len(d.models))
	for _, m := range d.models {
		out = append(out, cloneAzureModel(m))
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

func (d *azureRegistryData) getConnection(id string) (interfaces.AzureConnectionRecord, error) {
	i := d.connIndex(id)
	if i < 0 {
		return interfaces.AzureConnectionRecord{}, interfaces.ErrAzureConnectionNotFound
	}
	return cloneAzureConnection(d.connections[i]), nil
}

func (d *azureRegistryData) getModel(id string) (interfaces.AzureModelRecord, error) {
	i := d.modelIndex(id)
	if i < 0 {
		return interfaces.AzureModelRecord{}, interfaces.ErrAzureModelNotFound
	}
	return cloneAzureModel(d.models[i]), nil
}

// connectionConflict reports the uniqueness rule rec would break
// (the primary key, then the case-insensitive name), skipping row skip.
func (d *azureRegistryData) connectionConflict(rec interfaces.AzureConnectionRecord, skip int) error {
	name := strings.ToLower(rec.Name)
	for i, c := range d.connections {
		if i == skip {
			continue
		}
		if c.ID == rec.ID {
			return interfaces.ErrAzureDuplicateConnectionID
		}
		if strings.ToLower(c.Name) == name {
			return interfaces.ErrAzureDuplicateConnectionName
		}
	}
	return nil
}

// connectionNameTaken is connectionConflict's name rule on its own: whether a
// resource other than exceptID has name (strings.ToLower equality).
func (d *azureRegistryData) connectionNameTaken(name, exceptID string) bool {
	lower := strings.ToLower(name)
	for _, c := range d.connections {
		if c.ID != exceptID && strings.ToLower(c.Name) == lower {
			return true
		}
	}
	return false
}

// stamp is a creation time later than every CreatedAt already in d, so creation order stays total even
// for creates within one microsecond — the in-memory store is that fast; Postgres' clock moves between statements.
func (d *azureRegistryData) stamp() time.Time {
	now := azureNow()
	for _, c := range d.connections {
		if !now.After(c.CreatedAt) {
			now = c.CreatedAt.Add(time.Microsecond)
		}
	}
	for _, m := range d.models {
		if !now.After(m.CreatedAt) {
			now = m.CreatedAt.Add(time.Microsecond)
		}
	}
	return now
}

// newAzureConnection stamps a validated record for insertion at now.
func newAzureConnection(rec interfaces.AzureConnectionRecord, bootID string, now time.Time) interfaces.AzureConnectionRecord {
	rec.CreatedAt, rec.UpdatedAt = now, now
	rec.LastTest = nil
	rec.DiscoveryDismissedAt = nil
	if rec.Pending {
		rec.BootID = bootID
	} else {
		rec.BootID = ""
	}
	return rec
}

func (d *azureRegistryData) createConnection(rec interfaces.AzureConnectionRecord, pendingAllowed bool, bootID string) (interfaces.AzureConnectionRecord, error) {
	if rec.Pending && !pendingAllowed {
		return interfaces.AzureConnectionRecord{}, fmt.Errorf("%w: this store writes no pending rows", interfaces.ErrAzureRegistryInvalid)
	}
	if err := interfaces.ValidateAzureConnectionRecord(rec); err != nil {
		return interfaces.AzureConnectionRecord{}, err
	}
	if len(d.connections) >= interfaces.AzureMaxConnections {
		return interfaces.AzureConnectionRecord{}, interfaces.ErrAzureTooManyConnections
	}
	if err := d.connectionConflict(rec, -1); err != nil {
		return interfaces.AzureConnectionRecord{}, err
	}
	rec = newAzureConnection(rec, bootID, d.stamp())
	d.connections = append(d.connections, rec)
	return cloneAzureConnection(rec), nil
}

func (d *azureRegistryData) importConnection(rec interfaces.AzureConnectionRecord) (bool, error) {
	rec.Pending = false
	if err := interfaces.ValidateAzureConnectionRecord(rec); err != nil {
		return false, err
	}
	if len(d.connections) >= interfaces.AzureMaxConnections {
		return false, interfaces.ErrAzureTooManyConnections
	}
	if d.connectionConflict(rec, -1) != nil {
		return false, nil
	}
	d.connections = append(d.connections, newAzureConnection(rec, "", d.stamp()))
	return true, nil
}

func (d *azureRegistryData) updateConnection(id string, u interfaces.AzureConnectionUpdate) (interfaces.AzureConnectionRecord, error) {
	if u.Name != "" {
		if err := interfaces.ValidateAzureConnectionRecord(interfaces.AzureConnectionRecord{ID: interfaces.AzureDefaultConnectionID, Name: u.Name}); err != nil {
			return interfaces.AzureConnectionRecord{}, err
		}
	}
	i := d.connIndex(id)
	if i < 0 {
		return interfaces.AzureConnectionRecord{}, interfaces.ErrAzureConnectionNotFound
	}
	c := d.connections[i]
	if !u.ExpectedUpdatedAt.IsZero() && !azureTokenMatches(u.ExpectedUpdatedAt, c.UpdatedAt) {
		return interfaces.AzureConnectionRecord{}, interfaces.ErrAzureRegistryConflict
	}
	if u.Name != "" {
		probe := c
		probe.Name = u.Name
		if err := d.connectionConflict(probe, i); err != nil {
			return interfaces.AzureConnectionRecord{}, err
		}
		c.Name = u.Name
	}
	c.UpdatedAt = azureBump(c.UpdatedAt)
	d.connections[i] = c
	return cloneAzureConnection(c), nil
}

// finalizeConnection clears a pending row's flag; changed is false when the
// row was not pending (nothing to write).
func (d *azureRegistryData) finalizeConnection(id string) (changed bool, err error) {
	i := d.connIndex(id)
	if i < 0 {
		return false, interfaces.ErrAzureConnectionNotFound
	}
	changed = d.connections[i].Pending || d.connections[i].BootID != ""
	d.connections[i].Pending = false
	d.connections[i].BootID = ""
	return changed, nil
}

// setDiscoveryDismissed stamps (keeping an earlier stamp) or clears the
// dismissal; changed is false when the row already was in that state.
func (d *azureRegistryData) setDiscoveryDismissed(id string, dismissed bool) (rec interfaces.AzureConnectionRecord, changed bool, err error) {
	i := d.connIndex(id)
	if i < 0 {
		return interfaces.AzureConnectionRecord{}, false, interfaces.ErrAzureConnectionNotFound
	}
	switch {
	case !dismissed:
		changed = d.connections[i].DiscoveryDismissedAt != nil
		d.connections[i].DiscoveryDismissedAt = nil
	case d.connections[i].DiscoveryDismissedAt == nil:
		now := azureNow()
		d.connections[i].DiscoveryDismissedAt = &now
		changed = true
	}
	return cloneAzureConnection(d.connections[i]), changed, nil
}

func (d *azureRegistryData) recordConnectionTest(id string, result *interfaces.AzureTestResult) error {
	r, err := normalizeAzureTest(result)
	if err != nil {
		return err
	}
	i := d.connIndex(id)
	if i < 0 {
		return interfaces.ErrAzureConnectionNotFound
	}
	d.connections[i].LastTest = r
	return nil
}

// deleteConnection removes the resource and its models, returning the
// models as they were.
func (d *azureRegistryData) deleteConnection(id string) ([]interfaces.AzureModelRecord, error) {
	i := d.connIndex(id)
	if i < 0 {
		return nil, interfaces.ErrAzureConnectionNotFound
	}
	deleted := []interfaces.AzureModelRecord{}
	kept := d.models[:0:0]
	for _, m := range d.models {
		if m.ConnectionID == id {
			deleted = append(deleted, cloneAzureModel(m))
			continue
		}
		kept = append(kept, m)
	}
	d.models = kept
	d.connections = append(d.connections[:i:i], d.connections[i+1:]...)
	sort.SliceStable(deleted, func(a, b int) bool {
		if !deleted[a].CreatedAt.Equal(deleted[b].CreatedAt) {
			return deleted[a].CreatedAt.Before(deleted[b].CreatedAt)
		}
		return deleted[a].ID < deleted[b].ID
	})
	return deleted, nil
}

// modelConflict reports the uniqueness rule rec would break — the primary key, the deployment within its
// resource and namespace (case-insensitively), then a designation already held — skipping row skip.
func (d *azureRegistryData) modelConflict(rec interfaces.AzureModelRecord, skip int) error {
	ns := interfaces.AzureModelNamespace(rec.Kind)
	dep := strings.ToLower(rec.Deployment)
	rules := []struct {
		err   error
		match func(m interfaces.AzureModelRecord) bool
	}{
		{interfaces.ErrAzureDuplicateModelID, func(m interfaces.AzureModelRecord) bool { return m.ID == rec.ID }},
		{interfaces.ErrAzureDuplicateModel, func(m interfaces.AzureModelRecord) bool {
			return m.ConnectionID == rec.ConnectionID && interfaces.AzureModelNamespace(m.Kind) == ns && strings.ToLower(m.Deployment) == dep
		}},
		{interfaces.ErrAzureDuplicateDesignation, func(m interfaces.AzureModelRecord) bool {
			return rec.IsKnowledgeEmbedding && m.IsKnowledgeEmbedding || rec.IsDefault && m.IsDefault
		}},
	}
	for _, rule := range rules {
		for i, m := range d.models {
			if i != skip && rule.match(m) {
				return rule.err
			}
		}
	}
	return nil
}

// newAzureModel stamps a validated record for insertion at now.
func newAzureModel(rec interfaces.AzureModelRecord, now time.Time) interfaces.AzureModelRecord {
	rec.CreatedAt, rec.UpdatedAt = now, now
	rec.LastTest = nil
	return rec
}

func (d *azureRegistryData) createModel(owner interfaces.AzureOwner, rec interfaces.AzureModelRecord) (interfaces.AzureModelRecord, error) {
	if err := interfaces.ValidateAzureModelRecord(owner, rec); err != nil {
		return interfaces.AzureModelRecord{}, err
	}
	if len(d.models) >= interfaces.AzureMaxModels {
		return interfaces.AzureModelRecord{}, interfaces.ErrAzureTooManyModels
	}
	if err := d.modelConflict(rec, -1); err != nil {
		return interfaces.AzureModelRecord{}, err
	}
	if d.connIndex(rec.ConnectionID) < 0 {
		return interfaces.AzureModelRecord{}, interfaces.ErrAzureConnectionNotFound
	}
	rec = newAzureModel(rec, d.stamp())
	d.models = append(d.models, rec)
	return cloneAzureModel(rec), nil
}

func (d *azureRegistryData) importModel(owner interfaces.AzureOwner, rec interfaces.AzureModelRecord) (bool, error) {
	if err := interfaces.ValidateAzureModelRecord(owner, rec); err != nil {
		return false, err
	}
	if len(d.models) >= interfaces.AzureMaxModels {
		return false, interfaces.ErrAzureTooManyModels
	}
	if d.modelConflict(rec, -1) != nil {
		return false, nil
	}
	if d.connIndex(rec.ConnectionID) < 0 {
		return false, interfaces.ErrAzureConnectionNotFound
	}
	d.models = append(d.models, newAzureModel(rec, d.stamp()))
	return true, nil
}

func (d *azureRegistryData) updateModel(id string, u interfaces.AzureModelUpdate) (interfaces.AzureModelRecord, error) {
	if err := interfaces.ValidateAzureModelUpdate(u); err != nil {
		return interfaces.AzureModelRecord{}, err
	}
	i := d.modelIndex(id)
	if i < 0 {
		return interfaces.AzureModelRecord{}, interfaces.ErrAzureModelNotFound
	}
	m := d.models[i]
	if !u.ExpectedUpdatedAt.IsZero() && !azureTokenMatches(u.ExpectedUpdatedAt, m.UpdatedAt) {
		return interfaces.AzureModelRecord{}, interfaces.ErrAzureRegistryConflict
	}
	if m.IsKnowledgeEmbedding && u.Kind != interfaces.AzureKindEmbedding || m.IsDefault && u.Kind == interfaces.AzureKindEmbedding {
		return interfaces.AzureModelRecord{}, fmt.Errorf("%w: the new kind cannot hold the model's designation", interfaces.ErrAzureInvalidDesignation)
	}
	m.ConnectionID, m.Kind, m.Deployment = u.ConnectionID, u.Kind, u.Deployment
	m.DisplayName, m.Purpose, m.PurposeCustom, m.Description = u.DisplayName, u.Purpose, u.PurposeCustom, u.Description
	m.ContextWindow, m.MaxOutputTokens = u.ContextWindow, u.MaxOutputTokens
	if err := d.modelConflict(m, i); err != nil {
		return interfaces.AzureModelRecord{}, err
	}
	if d.connIndex(m.ConnectionID) < 0 {
		return interfaces.AzureModelRecord{}, interfaces.ErrAzureConnectionNotFound
	}
	m.UpdatedAt = azureBump(m.UpdatedAt)
	d.models[i] = m
	return cloneAzureModel(m), nil
}

func (d *azureRegistryData) recordModelTest(id string, result *interfaces.AzureTestResult) error {
	r, err := normalizeAzureTest(result)
	if err != nil {
		return err
	}
	i := d.modelIndex(id)
	if i < 0 {
		return interfaces.ErrAzureModelNotFound
	}
	d.models[i].LastTest = r
	return nil
}

func (d *azureRegistryData) deleteModel(id string) (interfaces.AzureModelRecord, error) {
	i := d.modelIndex(id)
	if i < 0 {
		return interfaces.AzureModelRecord{}, interfaces.ErrAzureModelNotFound
	}
	m := cloneAzureModel(d.models[i])
	d.models = append(d.models[:i:i], d.models[i+1:]...)
	return m, nil
}

func (d *azureRegistryData) setDesignation(owner interfaces.AzureOwner, desig interfaces.AzureDesignation, modelID string) error {
	var flag func(*interfaces.AzureModelRecord) *bool
	switch desig {
	case interfaces.AzureDesignationKnowledgeEmbedding:
		flag = func(m *interfaces.AzureModelRecord) *bool { return &m.IsKnowledgeEmbedding }
	case interfaces.AzureDesignationOrgDefault:
		if !owner.IsOrg() {
			return fmt.Errorf("%w: only organisations have a default for members", interfaces.ErrAzureInvalidDesignation)
		}
		flag = func(m *interfaces.AzureModelRecord) *bool { return &m.IsDefault }
	default:
		return fmt.Errorf("%w: designation %q", interfaces.ErrAzureRegistryInvalid, desig)
	}
	target := -1
	if modelID != "" {
		if target = d.modelIndex(modelID); target < 0 {
			return interfaces.ErrAzureModelNotFound
		}
		kind := d.models[target].Kind
		if desig == interfaces.AzureDesignationKnowledgeEmbedding && kind != interfaces.AzureKindEmbedding ||
			desig == interfaces.AzureDesignationOrgDefault && kind == interfaces.AzureKindEmbedding {
			return fmt.Errorf("%w: a %s model cannot hold %s", interfaces.ErrAzureInvalidDesignation, kind, desig)
		}
	}
	for i := range d.models {
		*flag(&d.models[i]) = i == target
	}
	return nil
}

func (d *azureRegistryData) counts() interfaces.AzureRegistryCounts {
	return interfaces.AzureRegistryCounts{Connections: len(d.connections), Models: len(d.models)}
}

func (d *azureRegistryData) markImported() time.Time {
	if d.importedAt == nil {
		now := azureNow()
		d.importedAt = &now
	}
	return *d.importedAt
}

// checkInvariants verifies loaded state against every rule a write would have
// enforced — the file store quarantines a file that breaks one rather than
// serving state no write path could have produced.
func (d *azureRegistryData) checkInvariants(owner interfaces.AzureOwner) error {
	for i, c := range d.connections {
		if err := interfaces.ValidateAzureConnectionRecord(c); err != nil {
			return err
		}
		if err := d.connectionConflict(c, i); err != nil {
			return fmt.Errorf("resource %q: %w", c.ID, err)
		}
	}
	for i, m := range d.models {
		if err := interfaces.ValidateAzureModelRecord(owner, m); err != nil {
			return err
		}
		if err := d.modelConflict(m, i); err != nil {
			return fmt.Errorf("model %q: %w", m.ID, err)
		}
		if d.connIndex(m.ConnectionID) < 0 {
			return fmt.Errorf("model %q: %w", m.ID, interfaces.ErrAzureConnectionNotFound)
		}
	}
	return nil
}

// ---- MemoryAzureRegistry ----

// MemoryAzureRegistry is an in-process AzureRegistryStore for any number of
// owners with the file store's rules (same engine) and no persistence; it
// backs the test fake, nothing in production. Transactional by default
// (PendingWrites false); EnablePendingWrites switches to the two-step create.
type MemoryAzureRegistry struct {
	mu            sync.Mutex
	owners        map[interfaces.AzureOwner]*azureRegistryData
	locks         map[interfaces.AzureOwner]*sync.Mutex
	pendingWrites bool
	bootID        string
	status        interfaces.AzureRegistryStatus
	hook          func(method string) error
}

var _ interfaces.AzureRegistryStore = (*MemoryAzureRegistry)(nil)

// NewMemoryAzureRegistry returns an empty in-memory registry.
func NewMemoryAzureRegistry() *MemoryAzureRegistry {
	return &MemoryAzureRegistry{
		owners: map[interfaces.AzureOwner]*azureRegistryData{},
		locks:  map[interfaces.AzureOwner]*sync.Mutex{},
		status: interfaces.AzureRegistryStatus{State: interfaces.AzureRegistryOK},
	}
}

// EnablePendingWrites makes the store behave like the file store: creates may
// be Pending, stamped with bootID, which BootID reports. Calling it again with
// another id simulates a restart (earlier rows keep theirs).
func (m *MemoryAzureRegistry) EnablePendingWrites(bootID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pendingWrites, m.bootID = true, bootID
}

// SetStatus sets what State reports.
func (m *MemoryAzureRegistry) SetStatus(status interfaces.AzureRegistryStatus) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.status = status
}

// SetHook installs fn, called with the method name at the start of every
// AzureRegistryStore method; a non-nil error is returned instead of running
// the method — failure injection for the fake.
func (m *MemoryAzureRegistry) SetHook(fn func(method string) error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.hook = fn
}

func (m *MemoryAzureRegistry) enter(method string) error {
	m.mu.Lock()
	hook := m.hook
	m.mu.Unlock()
	if hook != nil {
		return hook(method)
	}
	return nil
}

// with runs fn on owner's data under the store mutex.
func (m *MemoryAzureRegistry) with(method string, owner interfaces.AzureOwner, fn func(d *azureRegistryData) error) error {
	if err := m.enter(method); err != nil {
		return err
	}
	if err := owner.Validate(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	d := m.owners[owner]
	if d == nil {
		d = &azureRegistryData{}
		m.owners[owner] = d
	}
	return fn(d)
}

// State reports the status set by SetStatus (ok by default).
func (m *MemoryAzureRegistry) State(context.Context) interfaces.AzureRegistryStatus {
	_ = m.enter("State")
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.status
}

// PendingWrites reports whether EnablePendingWrites was called.
func (m *MemoryAzureRegistry) PendingWrites() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.pendingWrites
}

// BootID is the id EnablePendingWrites set ("" otherwise).
func (m *MemoryAzureRegistry) BootID() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.bootID
}

type memAzureLockKey struct {
	store *MemoryAzureRegistry
	owner interfaces.AzureOwner
}

// WithOwnerLock holds a per-owner mutex while fn runs; reentrant through
// fn's ctx, like the Postgres advisory lock within its transaction.
func (m *MemoryAzureRegistry) WithOwnerLock(ctx context.Context, owner interfaces.AzureOwner, fn func(ctx context.Context) error) error {
	if err := m.enter("WithOwnerLock"); err != nil {
		return err
	}
	if err := owner.Validate(); err != nil {
		return err
	}
	key := memAzureLockKey{m, owner}
	if ctx.Value(key) != nil {
		return fn(ctx)
	}
	m.mu.Lock()
	l := m.locks[owner]
	if l == nil {
		l = &sync.Mutex{}
		m.locks[owner] = l
	}
	m.mu.Unlock()
	l.Lock()
	defer l.Unlock()
	return fn(context.WithValue(ctx, key, true))
}

func (m *MemoryAzureRegistry) ListConnections(_ context.Context, owner interfaces.AzureOwner) (out []interfaces.AzureConnectionRecord, err error) {
	err = m.with("ListConnections", owner, func(d *azureRegistryData) error {
		out = d.listConnections()
		return nil
	})
	return out, err
}

func (m *MemoryAzureRegistry) GetConnection(_ context.Context, owner interfaces.AzureOwner, id string) (out interfaces.AzureConnectionRecord, err error) {
	err = m.with("GetConnection", owner, func(d *azureRegistryData) (e error) {
		out, e = d.getConnection(id)
		return e
	})
	return out, err
}

func (m *MemoryAzureRegistry) ConnectionNameTaken(_ context.Context, owner interfaces.AzureOwner, name, exceptID string) (taken bool, err error) {
	err = m.with("ConnectionNameTaken", owner, func(d *azureRegistryData) error {
		taken = d.connectionNameTaken(name, exceptID)
		return nil
	})
	return taken, err
}

func (m *MemoryAzureRegistry) CreateConnection(_ context.Context, owner interfaces.AzureOwner, rec interfaces.AzureConnectionRecord) (out interfaces.AzureConnectionRecord, err error) {
	err = m.with("CreateConnection", owner, func(d *azureRegistryData) (e error) {
		out, e = d.createConnection(rec, m.pendingWrites, m.bootID)
		return e
	})
	return out, err
}

func (m *MemoryAzureRegistry) ImportConnection(_ context.Context, owner interfaces.AzureOwner, rec interfaces.AzureConnectionRecord) (created bool, err error) {
	err = m.with("ImportConnection", owner, func(d *azureRegistryData) (e error) {
		created, e = d.importConnection(rec)
		return e
	})
	return created, err
}

func (m *MemoryAzureRegistry) UpdateConnection(_ context.Context, owner interfaces.AzureOwner, id string, u interfaces.AzureConnectionUpdate) (out interfaces.AzureConnectionRecord, err error) {
	err = m.with("UpdateConnection", owner, func(d *azureRegistryData) (e error) {
		out, e = d.updateConnection(id, u)
		return e
	})
	return out, err
}

func (m *MemoryAzureRegistry) FinalizeConnection(_ context.Context, owner interfaces.AzureOwner, id string) error {
	return m.with("FinalizeConnection", owner, func(d *azureRegistryData) error {
		_, err := d.finalizeConnection(id)
		return err
	})
}

func (m *MemoryAzureRegistry) SetDiscoveryDismissed(_ context.Context, owner interfaces.AzureOwner, id string, dismissed bool) (out interfaces.AzureConnectionRecord, err error) {
	err = m.with("SetDiscoveryDismissed", owner, func(d *azureRegistryData) (e error) {
		out, _, e = d.setDiscoveryDismissed(id, dismissed)
		return e
	})
	return out, err
}

func (m *MemoryAzureRegistry) RecordConnectionTest(_ context.Context, owner interfaces.AzureOwner, id string, result *interfaces.AzureTestResult) error {
	return m.with("RecordConnectionTest", owner, func(d *azureRegistryData) error {
		return d.recordConnectionTest(id, result)
	})
}

func (m *MemoryAzureRegistry) DeleteConnection(_ context.Context, owner interfaces.AzureOwner, id string) (out []interfaces.AzureModelRecord, err error) {
	err = m.with("DeleteConnection", owner, func(d *azureRegistryData) (e error) {
		out, e = d.deleteConnection(id)
		return e
	})
	return out, err
}

func (m *MemoryAzureRegistry) ListModels(_ context.Context, owner interfaces.AzureOwner) (out []interfaces.AzureModelRecord, err error) {
	err = m.with("ListModels", owner, func(d *azureRegistryData) error {
		out = d.listModels()
		return nil
	})
	return out, err
}

func (m *MemoryAzureRegistry) GetModel(_ context.Context, owner interfaces.AzureOwner, id string) (out interfaces.AzureModelRecord, err error) {
	err = m.with("GetModel", owner, func(d *azureRegistryData) (e error) {
		out, e = d.getModel(id)
		return e
	})
	return out, err
}

func (m *MemoryAzureRegistry) CreateModel(_ context.Context, owner interfaces.AzureOwner, rec interfaces.AzureModelRecord) (out interfaces.AzureModelRecord, err error) {
	err = m.with("CreateModel", owner, func(d *azureRegistryData) (e error) {
		out, e = d.createModel(owner, rec)
		return e
	})
	return out, err
}

func (m *MemoryAzureRegistry) ImportModel(_ context.Context, owner interfaces.AzureOwner, rec interfaces.AzureModelRecord) (created bool, err error) {
	err = m.with("ImportModel", owner, func(d *azureRegistryData) (e error) {
		created, e = d.importModel(owner, rec)
		return e
	})
	return created, err
}

func (m *MemoryAzureRegistry) UpdateModel(_ context.Context, owner interfaces.AzureOwner, id string, u interfaces.AzureModelUpdate) (out interfaces.AzureModelRecord, err error) {
	err = m.with("UpdateModel", owner, func(d *azureRegistryData) (e error) {
		out, e = d.updateModel(id, u)
		return e
	})
	return out, err
}

func (m *MemoryAzureRegistry) RecordModelTest(_ context.Context, owner interfaces.AzureOwner, id string, result *interfaces.AzureTestResult) error {
	return m.with("RecordModelTest", owner, func(d *azureRegistryData) error {
		return d.recordModelTest(id, result)
	})
}

func (m *MemoryAzureRegistry) DeleteModel(_ context.Context, owner interfaces.AzureOwner, id string) (out interfaces.AzureModelRecord, err error) {
	err = m.with("DeleteModel", owner, func(d *azureRegistryData) (e error) {
		out, e = d.deleteModel(id)
		return e
	})
	return out, err
}

func (m *MemoryAzureRegistry) SetDesignation(_ context.Context, owner interfaces.AzureOwner, desig interfaces.AzureDesignation, modelID string) error {
	return m.with("SetDesignation", owner, func(d *azureRegistryData) error {
		return d.setDesignation(owner, desig, modelID)
	})
}

func (m *MemoryAzureRegistry) Counts(_ context.Context, owner interfaces.AzureOwner) (out interfaces.AzureRegistryCounts, err error) {
	err = m.with("Counts", owner, func(d *azureRegistryData) error {
		out = d.counts()
		return nil
	})
	return out, err
}

func (m *MemoryAzureRegistry) ImportedAt(_ context.Context, owner interfaces.AzureOwner) (at time.Time, ok bool, err error) {
	err = m.with("ImportedAt", owner, func(d *azureRegistryData) error {
		if d.importedAt != nil {
			at, ok = *d.importedAt, true
		}
		return nil
	})
	return at, ok, err
}

func (m *MemoryAzureRegistry) MarkImported(_ context.Context, owner interfaces.AzureOwner) (at time.Time, err error) {
	err = m.with("MarkImported", owner, func(d *azureRegistryData) error {
		at = d.markImported()
		return nil
	})
	return at, err
}
