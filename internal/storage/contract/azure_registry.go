package contract

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"pad-analyzer/internal/storage/interfaces"
)

// AzureRegistryHarness adapts one AzureRegistryStore implementation to
// RunAzureRegistrySuite: Postgres (DB-gated), the azure-models.json file
// store and the test fake all run the same scenarios, so they cannot drift
// apart on what a duplicate, a limit, a designation or a missing row is.
type AzureRegistryHarness struct {
	// Fresh returns a store and an owner of the given kind whose registry
	// is empty. ok is false when the implementation cannot hold that kind
	// of owner (the file store has only the personal owner "").
	Fresh func(t *testing.T, kind interfaces.AzureOwnerKind) (store interfaces.AzureRegistryStore, owner interfaces.AzureOwner, ok bool)
	// Another returns a second, empty owner of the given kind on store for
	// the isolation scenario; nil, or ok false, when the store has a single
	// owner.
	Another func(t *testing.T, store interfaces.AzureRegistryStore, kind interfaces.AzureOwnerKind) (owner interfaces.AzureOwner, ok bool)
}

// RunAzureRegistrySuite runs the registry contract for both owner kinds.
func RunAzureRegistrySuite(t *testing.T, h AzureRegistryHarness) {
	t.Helper()
	for _, kind := range []interfaces.AzureOwnerKind{interfaces.AzureOwnerUser, interfaces.AzureOwnerOrg} {
		t.Run(string(kind), func(t *testing.T) {
			fresh := func(t *testing.T) (interfaces.AzureRegistryStore, interfaces.AzureOwner) {
				t.Helper()
				store, owner, ok := h.Fresh(t, kind)
				if !ok {
					t.Skipf("the store holds no %s owners", kind)
				}
				return store, owner
			}
			for _, sc := range azureScenarios {
				t.Run(sc.name, func(t *testing.T) {
					store, owner := fresh(t)
					sc.run(t, azureEnv{store: store, owner: owner, kind: kind, harness: h})
				})
			}
		})
	}
}

type azureEnv struct {
	store   interfaces.AzureRegistryStore
	owner   interfaces.AzureOwner
	kind    interfaces.AzureOwnerKind
	harness AzureRegistryHarness
}

var azureScenarios = []struct {
	name string
	run  func(t *testing.T, e azureEnv)
}{
	{"empty_lists_are_non_nil", azureEmptyLists},
	{"connection_crud", azureConnectionCRUD},
	{"connection_uniqueness", azureConnectionUniqueness},
	{"connection_name_taken_matches_the_writes", azureConnectionNameTaken},
	{"connection_validation", azureConnectionValidation},
	{"connection_compare_and_swap", azureConnectionCAS},
	{"stamps_do_not_move_updated_at", azureStamps},
	{"model_crud", azureModelCRUD},
	{"model_requires_its_resource", azureModelRequiresConnection},
	{"model_uniqueness", azureModelUniqueness},
	{"model_validation", azureModelValidation},
	{"model_compare_and_swap", azureModelCAS},
	{"delete_resource_cascades", azureDeleteCascades},
	{"limits", azureLimits},
	{"designations", azureDesignations},
	{"legacy_import", azureImport},
	{"owner_isolation", azureIsolation},
	{"owner_lock", azureOwnerLock},
	{"owner_lock_enforces_limit_concurrently", azureConcurrentLimit},
	{"pending_rows", azurePending},
	{"records_are_copies", azureCopies},
	{"state_ok", azureState},
}

// azureIDChars is the generated-id alphabet ([a-z0-9]).
const azureIDChars = "abcdefghijklmnopqrstuvwxyz0123456789"

// NewAzureTestID returns a random registry id ([a-z0-9]{10}).
func NewAzureTestID() string {
	var sb strings.Builder
	for range 10 {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(azureIDChars))))
		if err != nil {
			panic(err)
		}
		sb.WriteByte(azureIDChars[n.Int64()])
	}
	return sb.String()
}

func azureConn(id, name string) interfaces.AzureConnectionRecord {
	return interfaces.AzureConnectionRecord{ID: id, Name: name}
}

func azureModel(id, connID, kind, deployment string) interfaces.AzureModelRecord {
	return interfaces.AzureModelRecord{
		ID: id, ConnectionID: connID, Kind: kind, Deployment: deployment,
		DisplayName: deployment, Purpose: interfaces.AzurePurposeGeneral,
	}
}

func azureUpdateOf(m interfaces.AzureModelRecord) interfaces.AzureModelUpdate {
	return interfaces.AzureModelUpdate{
		ConnectionID: m.ConnectionID, Kind: m.Kind, Deployment: m.Deployment, DisplayName: m.DisplayName,
		Purpose: m.Purpose, PurposeCustom: m.PurposeCustom, Description: m.Description,
		ContextWindow: m.ContextWindow, MaxOutputTokens: m.MaxOutputTokens,
	}
}

func mustAzureConn(t *testing.T, e azureEnv, rec interfaces.AzureConnectionRecord) interfaces.AzureConnectionRecord {
	t.Helper()
	c, err := e.store.CreateConnection(context.Background(), e.owner, rec)
	if err != nil {
		t.Fatalf("CreateConnection(%s, %q): %v", rec.ID, rec.Name, err)
	}
	return c
}

func mustAzureModel(t *testing.T, e azureEnv, rec interfaces.AzureModelRecord) interfaces.AzureModelRecord {
	t.Helper()
	m, err := e.store.CreateModel(context.Background(), e.owner, rec)
	if err != nil {
		t.Fatalf("CreateModel(%s, %s %q): %v", rec.ID, rec.Kind, rec.Deployment, err)
	}
	return m
}

func wantAzureErr(t *testing.T, what string, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Errorf("%s: err = %v, want %v", what, err, want)
	}
}

func wantMicroseconds(t *testing.T, what string, ts time.Time) {
	t.Helper()
	if ts.IsZero() {
		t.Errorf("%s is zero", what)
	}
	if !ts.Equal(ts.Truncate(time.Microsecond)) {
		t.Errorf("%s = %v carries sub-microsecond precision the database cannot keep", what, ts)
	}
}

func azureEmptyLists(t *testing.T, e azureEnv) {
	ctx := context.Background()
	conns, err := e.store.ListConnections(ctx, e.owner)
	if err != nil || conns == nil || len(conns) != 0 {
		t.Errorf("ListConnections on an empty registry = (%v, %v), want a non-nil empty slice", conns, err)
	}
	models, err := e.store.ListModels(ctx, e.owner)
	if err != nil || models == nil || len(models) != 0 {
		t.Errorf("ListModels on an empty registry = (%v, %v), want a non-nil empty slice", models, err)
	}
	counts, err := e.store.Counts(ctx, e.owner)
	if err != nil || counts != (interfaces.AzureRegistryCounts{}) {
		t.Errorf("Counts on an empty registry = (%+v, %v)", counts, err)
	}
}

func azureConnectionCRUD(t *testing.T, e azureEnv) {
	ctx := context.Background()
	c := mustAzureConn(t, e, azureConn(interfaces.AzureDefaultConnectionID, "contoso-prod"))
	if c.ID != interfaces.AzureDefaultConnectionID || c.Name != "contoso-prod" || c.Pending || c.BootID != "" ||
		c.LastTest != nil || c.DiscoveryDismissedAt != nil {
		t.Errorf("created resource = %+v", c)
	}
	wantMicroseconds(t, "CreatedAt", c.CreatedAt)
	if !c.UpdatedAt.Equal(c.CreatedAt) {
		t.Errorf("a new resource's UpdatedAt %v differs from its CreatedAt %v", c.UpdatedAt, c.CreatedAt)
	}
	got, err := e.store.GetConnection(ctx, e.owner, c.ID)
	if err != nil || got.Name != c.Name || !got.CreatedAt.Equal(c.CreatedAt) || !got.UpdatedAt.Equal(c.UpdatedAt) {
		t.Errorf("GetConnection = (%+v, %v), want %+v", got, err, c)
	}

	id2 := NewAzureTestID()
	c2 := mustAzureConn(t, e, azureConn(id2, "Staging"))
	list, err := e.store.ListConnections(ctx, e.owner)
	if err != nil || len(list) != 2 || list[0].ID != c.ID || list[1].ID != id2 {
		t.Fatalf("ListConnections = (%+v, %v), want [default, %s] in creation order", list, err, id2)
	}

	renamed, err := e.store.UpdateConnection(ctx, e.owner, id2, interfaces.AzureConnectionUpdate{Name: "staging-eu"})
	if err != nil || renamed.Name != "staging-eu" || !renamed.CreatedAt.Equal(c2.CreatedAt) || !renamed.UpdatedAt.After(c2.UpdatedAt) {
		t.Errorf("rename = (%+v, %v), want the new name, the same CreatedAt and a later UpdatedAt than %v", renamed, err, c2.UpdatedAt)
	}
	wantMicroseconds(t, "UpdatedAt", renamed.UpdatedAt)
	touched, err := e.store.UpdateConnection(ctx, e.owner, id2, interfaces.AzureConnectionUpdate{})
	if err != nil || touched.Name != "staging-eu" || !touched.UpdatedAt.After(renamed.UpdatedAt) {
		t.Errorf("touch = (%+v, %v), want the name kept and UpdatedAt moved past %v", touched, err, renamed.UpdatedAt)
	}

	_, err = e.store.GetConnection(ctx, e.owner, "zzzzzzzzzz")
	wantAzureErr(t, "GetConnection(missing)", err, interfaces.ErrAzureConnectionNotFound)
	wantAzureErr(t, "GetConnection(missing) as ErrNotFound", err, interfaces.ErrNotFound)
	_, err = e.store.UpdateConnection(ctx, e.owner, "zzzzzzzzzz", interfaces.AzureConnectionUpdate{Name: "x"})
	wantAzureErr(t, "UpdateConnection(missing)", err, interfaces.ErrAzureConnectionNotFound)
}

func azureConnectionUniqueness(t *testing.T, e azureEnv) {
	ctx := context.Background()
	mustAzureConn(t, e, azureConn(interfaces.AzureDefaultConnectionID, "contoso-prod"))
	id2 := NewAzureTestID()
	mustAzureConn(t, e, azureConn(id2, "staging"))

	_, err := e.store.CreateConnection(ctx, e.owner, azureConn(interfaces.AzureDefaultConnectionID, "other"))
	wantAzureErr(t, "duplicate id", err, interfaces.ErrAzureDuplicateConnectionID)
	_, err = e.store.CreateConnection(ctx, e.owner, azureConn(NewAzureTestID(), "CONTOSO-Prod"))
	wantAzureErr(t, "duplicate name in another case", err, interfaces.ErrAzureDuplicateConnectionName)
	_, err = e.store.UpdateConnection(ctx, e.owner, id2, interfaces.AzureConnectionUpdate{Name: "Contoso-PROD"})
	wantAzureErr(t, "rename onto another resource's name", err, interfaces.ErrAzureDuplicateConnectionName)
	if c, err := e.store.UpdateConnection(ctx, e.owner, id2, interfaces.AzureConnectionUpdate{Name: "STAGING"}); err != nil || c.Name != "STAGING" {
		t.Errorf("renaming a resource to its own name in another case = (%+v, %v), want allowed", c, err)
	}
	if n, _ := e.store.Counts(ctx, e.owner); n.Connections != 2 {
		t.Errorf("refused writes changed the registry: %d resources, want 2", n.Connections)
	}
}

// azureConnectionNameTaken: ConnectionNameTaken answers exactly what the
// store's own writes enforce — whatever its case mapping (Postgres lower()
// under the database's collation) — so a caller that asks before a
// credential save is never refused by the write after it. Each probe is
// checked against a create (and a rename) of that name.
func azureConnectionNameTaken(t *testing.T, e azureEnv) {
	ctx := context.Background()
	own := mustAzureConn(t, e, azureConn(NewAzureTestID(), "Contoso-Prod"))
	mustAzureConn(t, e, azureConn(NewAzureTestID(), "Foi"))
	for _, tc := range []struct {
		name   string
		except string
		want   *bool // nil: whatever the store's case mapping says, as long as the writes agree
	}{
		{"CONTOSO-PROD", "", ptrBool(true)},
		{"contoso-prod", own.ID, ptrBool(false)}, // its own name, in another case
		{"contoso-dev", "", ptrBool(false)},
		{"FOI", "", ptrBool(true)},
		{"Foİ", "", nil}, // "Foİ": one name with "Foi" under en_US lower(), two under Go's folding
		{"ſoi", "", nil}, // "ſoi": Go folds ſ to s, lower() keeps it
	} {
		taken, err := e.store.ConnectionNameTaken(ctx, e.owner, tc.name, tc.except)
		if err != nil {
			t.Fatalf("ConnectionNameTaken(%q): %v", tc.name, err)
		}
		if tc.want != nil && taken != *tc.want {
			t.Errorf("ConnectionNameTaken(%q, except %q) = %v, want %v", tc.name, tc.except, taken, *tc.want)
		}
		if tc.except != "" {
			_, err := e.store.UpdateConnection(ctx, e.owner, tc.except, interfaces.AzureConnectionUpdate{Name: tc.name})
			if refused := errors.Is(err, interfaces.ErrAzureDuplicateConnectionName); refused != taken || (err != nil && !refused) {
				t.Errorf("renaming to %q: %v, but ConnectionNameTaken said %v", tc.name, err, taken)
			}
			continue
		}
		c, err := e.store.CreateConnection(ctx, e.owner, azureConn(NewAzureTestID(), tc.name))
		if refused := errors.Is(err, interfaces.ErrAzureDuplicateConnectionName); refused != taken || (err != nil && !refused) {
			t.Errorf("creating %q: %v, but ConnectionNameTaken said %v", tc.name, err, taken)
		}
		if err == nil {
			if _, err := e.store.DeleteConnection(ctx, e.owner, c.ID); err != nil {
				t.Fatalf("cleanup: %v", err)
			}
		}
	}
	if taken, err := e.store.ConnectionNameTaken(ctx, e.owner, "", ""); err != nil || taken {
		t.Errorf("ConnectionNameTaken(\"\") = (%v, %v), want no name taken", taken, err)
	}
}

func ptrBool(b bool) *bool { return &b }

func azureConnectionValidation(t *testing.T, e azureEnv) {
	ctx := context.Background()
	for _, tc := range []struct {
		what string
		rec  interfaces.AzureConnectionRecord
	}{
		{"an uppercase id", azureConn("Default", "a")},
		{"a short id", azureConn("abc", "a")},
		{"an 11-character id", azureConn("abc01234567", "a")},
		{"an id with a slash", azureConn("abc/012345", "a")},
		{"an empty name", azureConn(NewAzureTestID(), "")},
		{"a 65-character name", azureConn(NewAzureTestID(), strings.Repeat("n", 65))},
		{"a name with NUL", azureConn(NewAzureTestID(), "a\x00b")},
		{"a name that is not UTF-8", azureConn(NewAzureTestID(), "a\xffb")},
	} {
		_, err := e.store.CreateConnection(ctx, e.owner, tc.rec)
		wantAzureErr(t, "create with "+tc.what, err, interfaces.ErrAzureRegistryInvalid)
	}
	// Lengths count characters, not bytes: 64 two-byte characters fit.
	long := strings.Repeat("é", 64)
	if c, err := e.store.CreateConnection(ctx, e.owner, azureConn(NewAzureTestID(), long)); err != nil || c.Name != long {
		t.Errorf("a 64-character (128-byte) name = (%q, %v), want accepted", c.Name, err)
	}
	c := mustAzureConn(t, e, azureConn(NewAzureTestID(), "ok"))
	_, err := e.store.UpdateConnection(ctx, e.owner, c.ID, interfaces.AzureConnectionUpdate{Name: strings.Repeat("n", 65)})
	wantAzureErr(t, "rename to 65 characters", err, interfaces.ErrAzureRegistryInvalid)
}

func azureConnectionCAS(t *testing.T, e azureEnv) {
	ctx := context.Background()
	c := mustAzureConn(t, e, azureConn(NewAzureTestID(), "cas"))
	// The token as the API hands it out and parses it back.
	token, err := time.Parse(time.RFC3339Nano, c.UpdatedAt.Format(time.RFC3339Nano))
	if err != nil {
		t.Fatal(err)
	}
	u1, err := e.store.UpdateConnection(ctx, e.owner, c.ID, interfaces.AzureConnectionUpdate{Name: "cas-1", ExpectedUpdatedAt: token})
	if err != nil {
		t.Fatalf("update with the current token: %v", err)
	}
	_, err = e.store.UpdateConnection(ctx, e.owner, c.ID, interfaces.AzureConnectionUpdate{Name: "cas-stale", ExpectedUpdatedAt: token})
	wantAzureErr(t, "update with a stale token", err, interfaces.ErrAzureRegistryConflict)
	if got, _ := e.store.GetConnection(ctx, e.owner, c.ID); got.Name != "cas-1" {
		t.Errorf("a stale token wrote: name = %q", got.Name)
	}
	// Microsecond precision, as the database keeps it: nanoseconds within
	// the token's microsecond do not make it stale.
	if _, err := e.store.UpdateConnection(ctx, e.owner, c.ID, interfaces.AzureConnectionUpdate{Name: "cas-2", ExpectedUpdatedAt: u1.UpdatedAt.Add(999 * time.Nanosecond)}); err != nil {
		t.Errorf("update with the refreshed token (plus sub-microsecond noise): %v", err)
	}
	_, err = e.store.UpdateConnection(ctx, e.owner, "zzzzzzzzzz", interfaces.AzureConnectionUpdate{ExpectedUpdatedAt: token})
	wantAzureErr(t, "compare-and-swap on a missing resource", err, interfaces.ErrAzureConnectionNotFound)
}

func azureStamps(t *testing.T, e azureEnv) {
	ctx := context.Background()
	c := mustAzureConn(t, e, azureConn(NewAzureTestID(), "stamps"))
	d, err := e.store.SetDiscoveryDismissed(ctx, e.owner, c.ID, true)
	if err != nil || d.DiscoveryDismissedAt == nil || !d.UpdatedAt.Equal(c.UpdatedAt) {
		t.Fatalf("dismiss = (%+v, %v), want a stamp and UpdatedAt unchanged", d, err)
	}
	wantMicroseconds(t, "DiscoveryDismissedAt", *d.DiscoveryDismissedAt)
	if d2, err := e.store.SetDiscoveryDismissed(ctx, e.owner, c.ID, true); err != nil || d2.DiscoveryDismissedAt == nil || !d2.DiscoveryDismissedAt.Equal(*d.DiscoveryDismissedAt) {
		t.Errorf("dismissing again = (%+v, %v), want the first stamp kept", d2, err)
	}
	if d3, err := e.store.SetDiscoveryDismissed(ctx, e.owner, c.ID, false); err != nil || d3.DiscoveryDismissedAt != nil {
		t.Errorf("un-dismiss = (%+v, %v), want the stamp cleared", d3, err)
	}
	_, err = e.store.SetDiscoveryDismissed(ctx, e.owner, "zzzzzzzzzz", true)
	wantAzureErr(t, "dismiss a missing resource", err, interfaces.ErrAzureConnectionNotFound)

	if err := e.store.RecordConnectionTest(ctx, e.owner, c.ID, &interfaces.AzureTestResult{OK: true, LatencyMs: 42}); err != nil {
		t.Fatalf("RecordConnectionTest: %v", err)
	}
	got, _ := e.store.GetConnection(ctx, e.owner, c.ID)
	if got.LastTest == nil || !got.LastTest.OK || got.LastTest.LatencyMs != 42 || got.LastTest.At.IsZero() || !got.UpdatedAt.Equal(c.UpdatedAt) {
		t.Errorf("after a test: %+v (LastTest %+v), want the stamp with a time and UpdatedAt unchanged", got, got.LastTest)
	}
	at := time.Date(2026, 9, 30, 10, 11, 12, 123456789, time.UTC)
	long := strings.Repeat("é", interfaces.AzureTestErrorMaxLen+100)
	if err := e.store.RecordConnectionTest(ctx, e.owner, c.ID, &interfaces.AzureTestResult{At: at, Error: long}); err != nil {
		t.Fatalf("RecordConnectionTest(long error): %v", err)
	}
	got, _ = e.store.GetConnection(ctx, e.owner, c.ID)
	if got.LastTest == nil || got.LastTest.OK || utf8.RuneCountInString(got.LastTest.Error) != interfaces.AzureTestErrorMaxLen ||
		!got.LastTest.At.Equal(at.Truncate(time.Microsecond)) {
		t.Errorf("a long failure: %+v, want %d characters kept and the given time (to the microsecond)", got.LastTest, interfaces.AzureTestErrorMaxLen)
	}
	if err := e.store.RecordConnectionTest(ctx, e.owner, c.ID, nil); err != nil {
		t.Fatalf("clear test: %v", err)
	}
	if got, _ = e.store.GetConnection(ctx, e.owner, c.ID); got.LastTest != nil {
		t.Errorf("a nil result must clear the stamp, got %+v", got.LastTest)
	}
	wantAzureErr(t, "negative latency", e.store.RecordConnectionTest(ctx, e.owner, c.ID, &interfaces.AzureTestResult{LatencyMs: -1}), interfaces.ErrAzureRegistryInvalid)
	wantAzureErr(t, "test a missing resource", e.store.RecordConnectionTest(ctx, e.owner, "zzzzzzzzzz", &interfaces.AzureTestResult{OK: true}), interfaces.ErrAzureConnectionNotFound)

	m := mustAzureModel(t, e, azureModel(NewAzureTestID(), c.ID, interfaces.AzureKindChat, "gpt-4.1"))
	if err := e.store.RecordModelTest(ctx, e.owner, m.ID, &interfaces.AzureTestResult{OK: false, LatencyMs: 7, Error: "401"}); err != nil {
		t.Fatalf("RecordModelTest: %v", err)
	}
	gm, _ := e.store.GetModel(ctx, e.owner, m.ID)
	if gm.LastTest == nil || gm.LastTest.OK || gm.LastTest.Error != "401" || gm.LastTest.LatencyMs != 7 || !gm.UpdatedAt.Equal(m.UpdatedAt) {
		t.Errorf("model after a test: %+v (LastTest %+v)", gm, gm.LastTest)
	}
	if err := e.store.RecordModelTest(ctx, e.owner, m.ID, nil); err != nil {
		t.Fatalf("clear model test: %v", err)
	}
	if gm, _ = e.store.GetModel(ctx, e.owner, m.ID); gm.LastTest != nil {
		t.Errorf("a nil result must clear the model's stamp, got %+v", gm.LastTest)
	}
	wantAzureErr(t, "test a missing model", e.store.RecordModelTest(ctx, e.owner, "zzzzzzzzzz", &interfaces.AzureTestResult{}), interfaces.ErrAzureModelNotFound)
}

func azureModelCRUD(t *testing.T, e azureEnv) {
	ctx := context.Background()
	mustAzureConn(t, e, azureConn(interfaces.AzureDefaultConnectionID, "res"))
	rec := azureModel(NewAzureTestID(), interfaces.AzureDefaultConnectionID, interfaces.AzureKindChat, "gpt-4.1")
	rec.DisplayName, rec.Description = "GPT-4.1", "Everyday questions and flow help"
	rec.ContextWindow, rec.MaxOutputTokens = 1_000_000, 32_768
	m := mustAzureModel(t, e, rec)
	if m.ID != rec.ID || m.DisplayName != "GPT-4.1" || m.Description != rec.Description || m.ContextWindow != 1_000_000 ||
		m.MaxOutputTokens != 32_768 || m.Purpose != interfaces.AzurePurposeGeneral || m.LastTest != nil ||
		m.IsKnowledgeEmbedding || m.IsDefault {
		t.Errorf("created model = %+v", m)
	}
	wantMicroseconds(t, "model CreatedAt", m.CreatedAt)
	got, err := e.store.GetModel(ctx, e.owner, m.ID)
	if err != nil || got.Deployment != "gpt-4.1" || got.ContextWindow != 1_000_000 || !got.UpdatedAt.Equal(m.UpdatedAt) {
		t.Errorf("GetModel = (%+v, %v)", got, err)
	}

	u := azureUpdateOf(m)
	u.Kind, u.Deployment, u.DisplayName = interfaces.AzureKindClaude, "claude-sonnet-4-6", "Docs writer"
	u.Purpose, u.PurposeCustom, u.Description = interfaces.AzurePurposeCustom, "Docs", ""
	u.ContextWindow, u.MaxOutputTokens = 0, 0
	up, err := e.store.UpdateModel(ctx, e.owner, m.ID, u)
	if err != nil || up.ID != m.ID || up.Kind != interfaces.AzureKindClaude || up.Deployment != "claude-sonnet-4-6" ||
		up.PurposeCustom != "Docs" || up.ContextWindow != 0 || up.MaxOutputTokens != 0 || up.Description != "" ||
		!up.CreatedAt.Equal(m.CreatedAt) || !up.UpdatedAt.After(m.UpdatedAt) {
		t.Errorf("UpdateModel = (%+v, %v)", up, err)
	}
	list, err := e.store.ListModels(ctx, e.owner)
	if err != nil || len(list) != 1 || list[0].Deployment != "claude-sonnet-4-6" {
		t.Errorf("ListModels = (%+v, %v)", list, err)
	}

	del, err := e.store.DeleteModel(ctx, e.owner, m.ID)
	if err != nil || del.ID != m.ID || del.Deployment != "claude-sonnet-4-6" {
		t.Errorf("DeleteModel = (%+v, %v), want the model as it was", del, err)
	}
	_, err = e.store.GetModel(ctx, e.owner, m.ID)
	wantAzureErr(t, "GetModel after delete", err, interfaces.ErrAzureModelNotFound)
	wantAzureErr(t, "GetModel after delete as ErrNotFound", err, interfaces.ErrNotFound)
	_, err = e.store.DeleteModel(ctx, e.owner, m.ID)
	wantAzureErr(t, "DeleteModel twice", err, interfaces.ErrAzureModelNotFound)
	_, err = e.store.UpdateModel(ctx, e.owner, m.ID, u)
	wantAzureErr(t, "UpdateModel(missing)", err, interfaces.ErrAzureModelNotFound)
}

func azureModelRequiresConnection(t *testing.T, e azureEnv) {
	ctx := context.Background()
	_, err := e.store.CreateModel(ctx, e.owner, azureModel(NewAzureTestID(), "zzzzzzzzzz", interfaces.AzureKindChat, "gpt-4.1"))
	wantAzureErr(t, "a model on a missing resource", err, interfaces.ErrAzureConnectionNotFound)
	mustAzureConn(t, e, azureConn(interfaces.AzureDefaultConnectionID, "res"))
	m := mustAzureModel(t, e, azureModel(NewAzureTestID(), interfaces.AzureDefaultConnectionID, interfaces.AzureKindChat, "gpt-4.1"))
	u := azureUpdateOf(m)
	u.ConnectionID = "zzzzzzzzzz"
	_, err = e.store.UpdateModel(ctx, e.owner, m.ID, u)
	wantAzureErr(t, "moving a model to a missing resource", err, interfaces.ErrAzureConnectionNotFound)
	if got, _ := e.store.GetModel(ctx, e.owner, m.ID); got.ConnectionID != interfaces.AzureDefaultConnectionID {
		t.Errorf("a refused move changed the model: %+v", got)
	}
}

func azureModelUniqueness(t *testing.T, e azureEnv) {
	ctx := context.Background()
	mustAzureConn(t, e, azureConn(interfaces.AzureDefaultConnectionID, "res-a"))
	other := NewAzureTestID()
	mustAzureConn(t, e, azureConn(other, "res-b"))
	base := mustAzureModel(t, e, azureModel(NewAzureTestID(), interfaces.AzureDefaultConnectionID, interfaces.AzureKindChat, "gpt-4.1"))

	_, err := e.store.CreateModel(ctx, e.owner, azureModel(NewAzureTestID(), interfaces.AzureDefaultConnectionID, interfaces.AzureKindClaude, "GPT-4.1"))
	wantAzureErr(t, "claude shares chat's namespace, case-insensitively", err, interfaces.ErrAzureDuplicateModel)
	_, err = e.store.CreateModel(ctx, e.owner, azureModel(base.ID, other, interfaces.AzureKindChat, "o4-mini"))
	wantAzureErr(t, "duplicate model id", err, interfaces.ErrAzureDuplicateModelID)

	emb := mustAzureModel(t, e, azureModel(NewAzureTestID(), interfaces.AzureDefaultConnectionID, interfaces.AzureKindEmbedding, "gpt-4.1"))
	mustAzureModel(t, e, azureModel(NewAzureTestID(), interfaces.AzureDefaultConnectionID, interfaces.AzureKindAgent, "gpt-4.1"))
	mustAzureModel(t, e, azureModel(NewAzureTestID(), other, interfaces.AzureKindChat, "gpt-4.1"))

	u := azureUpdateOf(emb)
	u.Kind = interfaces.AzureKindChat
	_, err = e.store.UpdateModel(ctx, e.owner, emb.ID, u)
	wantAzureErr(t, "an update onto another model's deployment", err, interfaces.ErrAzureDuplicateModel)
	u = azureUpdateOf(base)
	u.Deployment = "GPT-4.1"
	if got, err := e.store.UpdateModel(ctx, e.owner, base.ID, u); err != nil || got.Deployment != "GPT-4.1" {
		t.Errorf("re-casing a model's own deployment = (%+v, %v), want allowed", got, err)
	}
	if n, _ := e.store.Counts(ctx, e.owner); n.Models != 4 {
		t.Errorf("models = %d, want 4 (refused writes must not land)", n.Models)
	}
}

func azureModelValidation(t *testing.T, e azureEnv) {
	ctx := context.Background()
	mustAzureConn(t, e, azureConn(interfaces.AzureDefaultConnectionID, "res"))
	ok := func() interfaces.AzureModelRecord {
		return azureModel(NewAzureTestID(), interfaces.AzureDefaultConnectionID, interfaces.AzureKindChat, "dep-"+NewAzureTestID())
	}
	with := func(mut func(m *interfaces.AzureModelRecord)) interfaces.AzureModelRecord {
		m := ok()
		mut(&m)
		return m
	}
	invalid := []struct {
		what string
		rec  interfaces.AzureModelRecord
		want error
	}{
		{"the id default", with(func(m *interfaces.AzureModelRecord) { m.ID = "default" }), interfaces.ErrAzureRegistryInvalid},
		{"an unknown kind", with(func(m *interfaces.AzureModelRecord) { m.Kind = "gpt" }), interfaces.ErrAzureRegistryInvalid},
		{"a deployment with a space", with(func(m *interfaces.AzureModelRecord) { m.Deployment = "gpt 4" }), interfaces.ErrAzureRegistryInvalid},
		{"a deployment with a slash", with(func(m *interfaces.AzureModelRecord) { m.Deployment = "a/b" }), interfaces.ErrAzureRegistryInvalid},
		{"a 65-character deployment", with(func(m *interfaces.AzureModelRecord) { m.Deployment = strings.Repeat("d", 65) }), interfaces.ErrAzureRegistryInvalid},
		{"a 129-character agent name", with(func(m *interfaces.AzureModelRecord) {
			m.Kind, m.Deployment = interfaces.AzureKindAgent, strings.Repeat("a", 129)
		}), interfaces.ErrAzureRegistryInvalid},
		{"an agent name with a tab", with(func(m *interfaces.AzureModelRecord) { m.Kind, m.Deployment = interfaces.AzureKindAgent, "bad\tname" }), interfaces.ErrAzureRegistryInvalid},
		{"an empty display name", with(func(m *interfaces.AzureModelRecord) { m.DisplayName = "" }), interfaces.ErrAzureRegistryInvalid},
		{"a 65-character display name", with(func(m *interfaces.AzureModelRecord) { m.DisplayName = strings.Repeat("x", 65) }), interfaces.ErrAzureRegistryInvalid},
		{"an unknown purpose", with(func(m *interfaces.AzureModelRecord) { m.Purpose = "docs" }), interfaces.ErrAzureRegistryInvalid},
		{"custom without text", with(func(m *interfaces.AzureModelRecord) { m.Purpose = interfaces.AzurePurposeCustom }), interfaces.ErrAzureRegistryInvalid},
		{"a preset with custom text", with(func(m *interfaces.AzureModelRecord) { m.PurposeCustom = "x" }), interfaces.ErrAzureRegistryInvalid},
		{"33 characters of custom purpose", with(func(m *interfaces.AzureModelRecord) {
			m.Purpose, m.PurposeCustom = interfaces.AzurePurposeCustom, strings.Repeat("p", 33)
		}), interfaces.ErrAzureRegistryInvalid},
		{"a 201-character description", with(func(m *interfaces.AzureModelRecord) { m.Description = strings.Repeat("d", 201) }), interfaces.ErrAzureRegistryInvalid},
		{"a context window of 1023", with(func(m *interfaces.AzureModelRecord) { m.ContextWindow = 1023 }), interfaces.ErrAzureRegistryInvalid},
		{"a context window of 2,000,001", with(func(m *interfaces.AzureModelRecord) { m.ContextWindow = 2_000_001 }), interfaces.ErrAzureRegistryInvalid},
		{"max output of 255", with(func(m *interfaces.AzureModelRecord) { m.MaxOutputTokens = 255 }), interfaces.ErrAzureRegistryInvalid},
		{"max output of 1,000,001", with(func(m *interfaces.AzureModelRecord) { m.MaxOutputTokens = 1_000_001 }), interfaces.ErrAzureRegistryInvalid},
		{"an embedding with a context window", with(func(m *interfaces.AzureModelRecord) {
			m.Kind, m.ContextWindow = interfaces.AzureKindEmbedding, 8192
		}), interfaces.ErrAzureRegistryInvalid},
		{"an embedding with max output", with(func(m *interfaces.AzureModelRecord) {
			m.Kind, m.MaxOutputTokens = interfaces.AzureKindEmbedding, 1024
		}), interfaces.ErrAzureRegistryInvalid},
		{"a chat model as the knowledge-base model", with(func(m *interfaces.AzureModelRecord) { m.IsKnowledgeEmbedding = true }), interfaces.ErrAzureInvalidDesignation},
		{"an embedding as the default for members", with(func(m *interfaces.AzureModelRecord) {
			m.Kind, m.IsDefault = interfaces.AzureKindEmbedding, true
		}), interfaces.ErrAzureInvalidDesignation},
	}
	if e.kind == interfaces.AzureOwnerUser {
		invalid = append(invalid, struct {
			what string
			rec  interfaces.AzureModelRecord
			want error
		}{"a user's default for members", with(func(m *interfaces.AzureModelRecord) { m.IsDefault = true }), interfaces.ErrAzureInvalidDesignation})
	}
	for _, tc := range invalid {
		_, err := e.store.CreateModel(ctx, e.owner, tc.rec)
		wantAzureErr(t, "create with "+tc.what, err, tc.want)
	}
	if n, _ := e.store.Counts(ctx, e.owner); n.Models != 0 {
		t.Fatalf("invalid models landed: %d", n.Models)
	}

	valid := []struct {
		what string
		rec  interfaces.AzureModelRecord
	}{
		{"the smallest limits", with(func(m *interfaces.AzureModelRecord) { m.ContextWindow, m.MaxOutputTokens = 1024, 256 })},
		{"the largest limits", with(func(m *interfaces.AzureModelRecord) { m.ContextWindow, m.MaxOutputTokens = 2_000_000, 1_000_000 })},
		{"a 128-character agent name with spaces", with(func(m *interfaces.AzureModelRecord) {
			m.Kind, m.Deployment = interfaces.AzureKindAgent, "Review bot "+strings.Repeat("r", 117)
		})},
		{"32 two-byte characters of custom purpose", with(func(m *interfaces.AzureModelRecord) {
			m.Purpose, m.PurposeCustom = interfaces.AzurePurposeCustom, strings.Repeat("ü", 32)
		})},
		{"a 200-character two-byte description", with(func(m *interfaces.AzureModelRecord) { m.Description = strings.Repeat("ß", 200) })},
		{"a 64-character deployment", with(func(m *interfaces.AzureModelRecord) { m.Deployment = "A._-" + strings.Repeat("z", 60) })},
	}
	for _, tc := range valid {
		if _, err := e.store.CreateModel(ctx, e.owner, tc.rec); err != nil {
			t.Errorf("create with %s: %v, want accepted", tc.what, err)
		}
	}

	m := mustAzureModel(t, e, ok())
	u := azureUpdateOf(m)
	u.Kind = "gpt"
	_, err := e.store.UpdateModel(ctx, e.owner, m.ID, u)
	wantAzureErr(t, "update to an unknown kind", err, interfaces.ErrAzureRegistryInvalid)
}

func azureModelCAS(t *testing.T, e azureEnv) {
	ctx := context.Background()
	mustAzureConn(t, e, azureConn(interfaces.AzureDefaultConnectionID, "res"))
	m := mustAzureModel(t, e, azureModel(NewAzureTestID(), interfaces.AzureDefaultConnectionID, interfaces.AzureKindChat, "gpt-4.1"))
	u := azureUpdateOf(m)
	u.DisplayName, u.ExpectedUpdatedAt = "first", m.UpdatedAt
	up, err := e.store.UpdateModel(ctx, e.owner, m.ID, u)
	if err != nil {
		t.Fatalf("update with the current token: %v", err)
	}
	u.DisplayName = "stale"
	_, err = e.store.UpdateModel(ctx, e.owner, m.ID, u)
	wantAzureErr(t, "update with a stale token", err, interfaces.ErrAzureRegistryConflict)
	u.DisplayName, u.ExpectedUpdatedAt = "second", up.UpdatedAt
	if _, err := e.store.UpdateModel(ctx, e.owner, m.ID, u); err != nil {
		t.Errorf("update with the refreshed token: %v", err)
	}
}

func azureDeleteCascades(t *testing.T, e azureEnv) {
	ctx := context.Background()
	a, b := NewAzureTestID(), NewAzureTestID()
	mustAzureConn(t, e, azureConn(a, "res-a"))
	mustAzureConn(t, e, azureConn(b, "res-b"))
	a1 := mustAzureModel(t, e, azureModel(NewAzureTestID(), a, interfaces.AzureKindChat, "gpt-4.1"))
	a2 := mustAzureModel(t, e, azureModel(NewAzureTestID(), a, interfaces.AzureKindEmbedding, "text-embedding-3-large"))
	b1 := mustAzureModel(t, e, azureModel(NewAzureTestID(), b, interfaces.AzureKindChat, "gpt-4.1"))
	if err := e.store.SetDesignation(ctx, e.owner, interfaces.AzureDesignationKnowledgeEmbedding, a2.ID); err != nil {
		t.Fatalf("SetDesignation: %v", err)
	}

	deleted, err := e.store.DeleteConnection(ctx, e.owner, a)
	if err != nil || len(deleted) != 2 {
		t.Fatalf("DeleteConnection = (%+v, %v), want its two models", deleted, err)
	}
	ids := map[string]bool{deleted[0].ID: true, deleted[1].ID: true}
	if !ids[a1.ID] || !ids[a2.ID] {
		t.Errorf("deleted models = %+v, want %s and %s", deleted, a1.ID, a2.ID)
	}
	var sawKB bool
	for _, m := range deleted {
		sawKB = sawKB || m.IsKnowledgeEmbedding
	}
	if !sawKB {
		t.Error("the deleted models must report the knowledge-base designation they held")
	}
	models, _ := e.store.ListModels(ctx, e.owner)
	if len(models) != 1 || models[0].ID != b1.ID {
		t.Errorf("models after the cascade = %+v, want only %s", models, b1.ID)
	}
	_, err = e.store.GetConnection(ctx, e.owner, a)
	wantAzureErr(t, "the deleted resource", err, interfaces.ErrAzureConnectionNotFound)
	_, err = e.store.DeleteConnection(ctx, e.owner, a)
	wantAzureErr(t, "deleting it again", err, interfaces.ErrAzureConnectionNotFound)
	if deleted, err := e.store.DeleteConnection(ctx, e.owner, b); err != nil || len(deleted) != 1 {
		t.Errorf("deleting the other resource = (%+v, %v)", deleted, err)
	}
	if n, _ := e.store.Counts(ctx, e.owner); n != (interfaces.AzureRegistryCounts{}) {
		t.Errorf("counts after deleting everything = %+v", n)
	}
}

func azureLimits(t *testing.T, e azureEnv) {
	ctx := context.Background()
	var first string
	for i := range interfaces.AzureMaxConnections {
		c := mustAzureConn(t, e, azureConn(NewAzureTestID(), fmt.Sprintf("res-%02d", i)))
		if first == "" {
			first = c.ID
		}
	}
	_, err := e.store.CreateConnection(ctx, e.owner, azureConn(NewAzureTestID(), "one-too-many"))
	wantAzureErr(t, "resource over the limit", err, interfaces.ErrAzureTooManyConnections)
	_, err = e.store.ImportConnection(ctx, e.owner, azureConn(NewAzureTestID(), "imported-too-many"))
	wantAzureErr(t, "imported resource over the limit", err, interfaces.ErrAzureTooManyConnections)

	for i := range interfaces.AzureMaxModels {
		mustAzureModel(t, e, azureModel(NewAzureTestID(), first, interfaces.AzureKindChat, fmt.Sprintf("dep-%02d", i)))
	}
	_, err = e.store.CreateModel(ctx, e.owner, azureModel(NewAzureTestID(), first, interfaces.AzureKindChat, "dep-over"))
	wantAzureErr(t, "model over the limit", err, interfaces.ErrAzureTooManyModels)
	_, err = e.store.ImportModel(ctx, e.owner, azureModel(NewAzureTestID(), first, interfaces.AzureKindChat, "dep-over"))
	wantAzureErr(t, "imported model over the limit", err, interfaces.ErrAzureTooManyModels)
	if n, err := e.store.Counts(ctx, e.owner); err != nil || n.Connections != interfaces.AzureMaxConnections || n.Models != interfaces.AzureMaxModels {
		t.Errorf("Counts at the limits = (%+v, %v)", n, err)
	}
}

func azureDesignations(t *testing.T, e azureEnv) {
	ctx := context.Background()
	conn := NewAzureTestID()
	mustAzureConn(t, e, azureConn(conn, "res"))
	e1 := mustAzureModel(t, e, azureModel(NewAzureTestID(), conn, interfaces.AzureKindEmbedding, "embed-a"))
	e2 := mustAzureModel(t, e, azureModel(NewAzureTestID(), conn, interfaces.AzureKindEmbedding, "embed-b"))
	c1 := mustAzureModel(t, e, azureModel(NewAzureTestID(), conn, interfaces.AzureKindChat, "gpt-4.1"))
	holder := func(d interfaces.AzureDesignation) []string {
		t.Helper()
		models, err := e.store.ListModels(ctx, e.owner)
		if err != nil {
			t.Fatalf("ListModels: %v", err)
		}
		var out []string
		for _, m := range models {
			if d == interfaces.AzureDesignationKnowledgeEmbedding && m.IsKnowledgeEmbedding || d == interfaces.AzureDesignationOrgDefault && m.IsDefault {
				out = append(out, m.ID)
			}
		}
		return out
	}
	kb := interfaces.AzureDesignationKnowledgeEmbedding
	if err := e.store.SetDesignation(ctx, e.owner, kb, e1.ID); err != nil {
		t.Fatalf("designate e1: %v", err)
	}
	if got := holder(kb); len(got) != 1 || got[0] != e1.ID {
		t.Errorf("knowledge-base holder = %v, want [%s]", got, e1.ID)
	}
	if err := e.store.SetDesignation(ctx, e.owner, kb, e2.ID); err != nil {
		t.Fatalf("move to e2: %v", err)
	}
	if got := holder(kb); len(got) != 1 || got[0] != e2.ID {
		t.Errorf("after the move = %v, want only [%s]", got, e2.ID)
	}
	wantAzureErr(t, "a chat model as the knowledge-base model", e.store.SetDesignation(ctx, e.owner, kb, c1.ID), interfaces.ErrAzureInvalidDesignation)
	wantAzureErr(t, "a missing model", e.store.SetDesignation(ctx, e.owner, kb, "zzzzzzzzzz"), interfaces.ErrAzureModelNotFound)
	if got := holder(kb); len(got) != 1 || got[0] != e2.ID {
		t.Errorf("a refused move changed the holder: %v, want [%s]", got, e2.ID)
	}
	e3 := azureModel(NewAzureTestID(), conn, interfaces.AzureKindEmbedding, "embed-c")
	e3.IsKnowledgeEmbedding = true
	_, err := e.store.CreateModel(ctx, e.owner, e3)
	wantAzureErr(t, "creating a second knowledge-base model", err, interfaces.ErrAzureDuplicateDesignation)
	if created, err := e.store.ImportModel(ctx, e.owner, e3); err != nil || created {
		t.Errorf("importing a second knowledge-base model = (%v, %v), want skipped", created, err)
	}
	u := azureUpdateOf(e2)
	u.Kind, u.Deployment = interfaces.AzureKindChat, "now-chat"
	_, err = e.store.UpdateModel(ctx, e.owner, e2.ID, u)
	wantAzureErr(t, "turning the knowledge-base model into a chat model", err, interfaces.ErrAzureInvalidDesignation)
	if err := e.store.SetDesignation(ctx, e.owner, kb, ""); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if got := holder(kb); len(got) != 0 {
		t.Errorf("after clearing = %v, want none", got)
	}
	wantAzureErr(t, "an unknown designation", e.store.SetDesignation(ctx, e.owner, "favourite", c1.ID), interfaces.ErrAzureRegistryInvalid)

	def := interfaces.AzureDesignationOrgDefault
	if e.kind == interfaces.AzureOwnerUser {
		wantAzureErr(t, "a user's default for members", e.store.SetDesignation(ctx, e.owner, def, c1.ID), interfaces.ErrAzureInvalidDesignation)
		return
	}
	if err := e.store.SetDesignation(ctx, e.owner, def, c1.ID); err != nil {
		t.Fatalf("org default: %v", err)
	}
	wantAzureErr(t, "an embedding as the org default", e.store.SetDesignation(ctx, e.owner, def, e1.ID), interfaces.ErrAzureInvalidDesignation)
	c2 := azureModel(NewAzureTestID(), conn, interfaces.AzureKindAgent, "review-bot")
	c2.IsDefault = true
	_, err = e.store.CreateModel(ctx, e.owner, c2)
	wantAzureErr(t, "creating a second org default", err, interfaces.ErrAzureDuplicateDesignation)
	c2.IsDefault = false
	c2m := mustAzureModel(t, e, c2)
	if err := e.store.SetDesignation(ctx, e.owner, def, c2m.ID); err != nil {
		t.Fatalf("move the org default: %v", err)
	}
	if got := holder(def); len(got) != 1 || got[0] != c2m.ID {
		t.Errorf("org default holder = %v, want [%s]", got, c2m.ID)
	}
	u = azureUpdateOf(c2m)
	u.Kind = interfaces.AzureKindEmbedding
	_, err = e.store.UpdateModel(ctx, e.owner, c2m.ID, u)
	wantAzureErr(t, "turning the org default into an embedding model", err, interfaces.ErrAzureInvalidDesignation)
	if err := e.store.SetDesignation(ctx, e.owner, def, ""); err != nil || len(holder(def)) != 0 {
		t.Errorf("clearing the org default: %v, holders %v", err, holder(def))
	}
}

func azureImport(t *testing.T, e azureEnv) {
	ctx := context.Background()
	if _, ok, err := e.store.ImportedAt(ctx, e.owner); err != nil || ok {
		t.Fatalf("ImportedAt on a fresh registry = (%v, %v), want not imported", ok, err)
	}
	def := azureConn(interfaces.AzureDefaultConnectionID, "my-res")
	if created, err := e.store.ImportConnection(ctx, e.owner, def); err != nil || !created {
		t.Fatalf("import the default resource = (%v, %v)", created, err)
	}
	if created, err := e.store.ImportConnection(ctx, e.owner, def); err != nil || created {
		t.Errorf("import it again = (%v, %v), want skipped", created, err)
	}
	if created, err := e.store.ImportConnection(ctx, e.owner, azureConn(NewAzureTestID(), "MY-RES")); err != nil || created {
		t.Errorf("import a name clash = (%v, %v), want skipped", created, err)
	}
	chat := azureModel(NewAzureTestID(), interfaces.AzureDefaultConnectionID, interfaces.AzureKindChat, "gpt-4.1")
	if created, err := e.store.ImportModel(ctx, e.owner, chat); err != nil || !created {
		t.Fatalf("import a model = (%v, %v)", created, err)
	}
	if created, err := e.store.ImportModel(ctx, e.owner, chat); err != nil || created {
		t.Errorf("import the same model again = (%v, %v), want skipped", created, err)
	}
	if created, err := e.store.ImportModel(ctx, e.owner, azureModel(NewAzureTestID(), interfaces.AzureDefaultConnectionID, interfaces.AzureKindClaude, "GPT-4.1")); err != nil || created {
		t.Errorf("import a deployment clash = (%v, %v), want skipped", created, err)
	}
	_, err := e.store.ImportModel(ctx, e.owner, azureModel(NewAzureTestID(), "zzzzzzzzzz", interfaces.AzureKindChat, "o4-mini"))
	wantAzureErr(t, "import onto a missing resource", err, interfaces.ErrAzureConnectionNotFound)
	_, err = e.store.ImportModel(ctx, e.owner, azureModel(NewAzureTestID(), interfaces.AzureDefaultConnectionID, "gpt", "o4-mini"))
	wantAzureErr(t, "import an invalid model", err, interfaces.ErrAzureRegistryInvalid)

	at, err := e.store.MarkImported(ctx, e.owner)
	if err != nil || at.IsZero() {
		t.Fatalf("MarkImported = (%v, %v)", at, err)
	}
	wantMicroseconds(t, "import marker", at)
	if got, ok, err := e.store.ImportedAt(ctx, e.owner); err != nil || !ok || !got.Equal(at) {
		t.Errorf("ImportedAt = (%v, %v, %v), want %v", got, ok, err, at)
	}
	if again, err := e.store.MarkImported(ctx, e.owner); err != nil || !again.Equal(at) {
		t.Errorf("MarkImported again = (%v, %v), want the first marker %v kept", again, err, at)
	}
	// The marker outlives the default resource, so it is never re-imported.
	if _, err := e.store.DeleteConnection(ctx, e.owner, interfaces.AzureDefaultConnectionID); err != nil {
		t.Fatalf("delete the default resource: %v", err)
	}
	if _, ok, _ := e.store.ImportedAt(ctx, e.owner); !ok {
		t.Error("deleting the default resource dropped the import marker")
	}
}

func azureIsolation(t *testing.T, e azureEnv) {
	if e.harness.Another == nil {
		t.Skip("the store has a single owner")
	}
	other, ok := e.harness.Another(t, e.store, e.kind)
	if !ok {
		t.Skip("the store has a single owner")
	}
	ctx := context.Background()
	c := mustAzureConn(t, e, azureConn(interfaces.AzureDefaultConnectionID, "mine"))
	m := mustAzureModel(t, e, azureModel(NewAzureTestID(), c.ID, interfaces.AzureKindEmbedding, "embed"))

	if _, err := e.store.GetConnection(ctx, other, c.ID); !errors.Is(err, interfaces.ErrAzureConnectionNotFound) {
		t.Errorf("another owner read the resource: %v", err)
	}
	if _, err := e.store.GetModel(ctx, other, m.ID); !errors.Is(err, interfaces.ErrAzureModelNotFound) {
		t.Errorf("another owner read the model: %v", err)
	}
	if list, _ := e.store.ListConnections(ctx, other); len(list) != 0 {
		t.Errorf("another owner lists %d resources", len(list))
	}
	if list, _ := e.store.ListModels(ctx, other); len(list) != 0 {
		t.Errorf("another owner lists %d models", len(list))
	}
	if n, _ := e.store.Counts(ctx, other); n != (interfaces.AzureRegistryCounts{}) {
		t.Errorf("another owner's counts = %+v", n)
	}
	if _, err := e.store.UpdateConnection(ctx, other, c.ID, interfaces.AzureConnectionUpdate{Name: "stolen"}); !errors.Is(err, interfaces.ErrAzureConnectionNotFound) {
		t.Errorf("another owner renamed the resource: %v", err)
	}
	if err := e.store.RecordConnectionTest(ctx, other, c.ID, &interfaces.AzureTestResult{OK: true}); !errors.Is(err, interfaces.ErrAzureConnectionNotFound) {
		t.Errorf("another owner stamped the resource: %v", err)
	}
	if err := e.store.SetDesignation(ctx, other, interfaces.AzureDesignationKnowledgeEmbedding, m.ID); !errors.Is(err, interfaces.ErrAzureModelNotFound) {
		t.Errorf("another owner designated the model: %v", err)
	}
	if _, err := e.store.DeleteModel(ctx, other, m.ID); !errors.Is(err, interfaces.ErrAzureModelNotFound) {
		t.Errorf("another owner deleted the model: %v", err)
	}
	if _, err := e.store.DeleteConnection(ctx, other, c.ID); !errors.Is(err, interfaces.ErrAzureConnectionNotFound) {
		t.Errorf("another owner deleted the resource: %v", err)
	}
	// Uniqueness is per owner: the other owner may reuse ids and names.
	if _, err := e.store.CreateConnection(ctx, other, azureConn(c.ID, "mine")); err != nil {
		t.Errorf("another owner reusing the id and name: %v", err)
	}
	if _, err := e.store.CreateModel(ctx, other, azureModel(m.ID, c.ID, interfaces.AzureKindEmbedding, "embed")); err != nil {
		t.Errorf("another owner reusing the model id and deployment: %v", err)
	}
	if got, err := e.store.GetConnection(ctx, e.owner, c.ID); err != nil || got.Name != "mine" {
		t.Errorf("the first owner's resource = (%+v, %v), want it untouched", got, err)
	}
	if got, err := e.store.GetModel(ctx, e.owner, m.ID); err != nil || got.IsKnowledgeEmbedding {
		t.Errorf("the first owner's model = (%+v, %v), want it untouched", got, err)
	}
}

func azureOwnerLock(t *testing.T, e azureEnv) {
	ctx := context.Background()
	id := NewAzureTestID()
	called, nested := false, false
	err := e.store.WithOwnerLock(ctx, e.owner, func(ctx context.Context) error {
		called = true
		if _, err := e.store.CreateConnection(ctx, e.owner, azureConn(id, "locked")); err != nil {
			return err
		}
		// Reentrant through ctx, like an advisory lock within its session.
		return e.store.WithOwnerLock(ctx, e.owner, func(ctx context.Context) error {
			nested = true
			_, err := e.store.GetConnection(ctx, e.owner, id)
			return err
		})
	})
	if err != nil || !called || !nested {
		t.Fatalf("WithOwnerLock = %v (called %v, nested %v)", err, called, nested)
	}
	if _, err := e.store.GetConnection(ctx, e.owner, id); err != nil {
		t.Errorf("a write under the lock did not persist: %v", err)
	}
	boom := errors.New("boom")
	if err := e.store.WithOwnerLock(ctx, e.owner, func(context.Context) error { return boom }); !errors.Is(err, boom) {
		t.Errorf("WithOwnerLock must return fn's error, got %v", err)
	}
}

// azureConcurrentLimit is the service's create path in miniature: count,
// then create, under the owner lock. Racing creators must never exceed
// the limit, and every refusal must be the typed limit error.
func azureConcurrentLimit(t *testing.T, e azureEnv) {
	const racers = interfaces.AzureMaxConnections + 6
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		created int
		refused int
		other   []error
	)
	for i := range racers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			err := e.store.WithOwnerLock(context.Background(), e.owner, func(ctx context.Context) error {
				n, err := e.store.Counts(ctx, e.owner)
				if err != nil {
					return err
				}
				if n.Connections >= interfaces.AzureMaxConnections {
					return interfaces.ErrAzureTooManyConnections
				}
				_, err = e.store.CreateConnection(ctx, e.owner, azureConn(NewAzureTestID(), fmt.Sprintf("racer-%02d", i)))
				return err
			})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				created++
			case errors.Is(err, interfaces.ErrAzureTooManyConnections):
				refused++
			default:
				other = append(other, err)
			}
		}(i)
	}
	wg.Wait()
	if len(other) > 0 {
		t.Fatalf("unexpected errors: %v", other)
	}
	if created != interfaces.AzureMaxConnections || refused != racers-interfaces.AzureMaxConnections {
		t.Errorf("created %d, refused %d; want %d and %d", created, refused, interfaces.AzureMaxConnections, racers-interfaces.AzureMaxConnections)
	}
	if n, _ := e.store.Counts(context.Background(), e.owner); n.Connections != interfaces.AzureMaxConnections {
		t.Errorf("resources = %d, want exactly the limit", n.Connections)
	}
}

func azurePending(t *testing.T, e azureEnv) {
	ctx := context.Background()
	id := NewAzureTestID()
	if !e.store.PendingWrites() {
		if e.store.BootID() != "" {
			t.Errorf("BootID = %q on a store without pending writes", e.store.BootID())
		}
		_, err := e.store.CreateConnection(ctx, e.owner, interfaces.AzureConnectionRecord{ID: id, Name: "pending", Pending: true})
		wantAzureErr(t, "a pending row on a transactional store", err, interfaces.ErrAzureRegistryInvalid)
		mustAzureConn(t, e, azureConn(id, "plain"))
		if err := e.store.FinalizeConnection(ctx, e.owner, id); err != nil {
			t.Errorf("FinalizeConnection on a stored row: %v", err)
		}
		wantAzureErr(t, "FinalizeConnection(missing)", e.store.FinalizeConnection(ctx, e.owner, "zzzzzzzzzz"), interfaces.ErrAzureConnectionNotFound)
		return
	}
	boot := e.store.BootID()
	if boot == "" {
		t.Fatal("a store with pending writes must report a BootID")
	}
	c, err := e.store.CreateConnection(ctx, e.owner, interfaces.AzureConnectionRecord{ID: id, Name: "pending", Pending: true, BootID: "caller-supplied"})
	if err != nil || !c.Pending || c.BootID != boot {
		t.Fatalf("pending create = (%+v, %v), want Pending with the store's BootID %q", c, err, boot)
	}
	list, _ := e.store.ListConnections(ctx, e.owner)
	if len(list) != 1 || !list[0].Pending || list[0].BootID != boot {
		t.Errorf("ListConnections = %+v, want the pending row with its boot id", list)
	}
	if n, _ := e.store.Counts(ctx, e.owner); n.Connections != 1 {
		t.Errorf("a pending row must count toward the limit, counts = %+v", n)
	}
	if err := e.store.FinalizeConnection(ctx, e.owner, id); err != nil {
		t.Fatalf("FinalizeConnection: %v", err)
	}
	if got, _ := e.store.GetConnection(ctx, e.owner, id); got.Pending || got.BootID != "" {
		t.Errorf("after finalising: %+v, want Pending false and no boot id", got)
	}
	plain := mustAzureConn(t, e, azureConn(NewAzureTestID(), "plain"))
	if plain.Pending || plain.BootID != "" {
		t.Errorf("a non-pending create = %+v, want no pending state", plain)
	}
	wantAzureErr(t, "FinalizeConnection(missing)", e.store.FinalizeConnection(ctx, e.owner, "zzzzzzzzzz"), interfaces.ErrAzureConnectionNotFound)
}

func azureCopies(t *testing.T, e azureEnv) {
	ctx := context.Background()
	c := mustAzureConn(t, e, azureConn(NewAzureTestID(), "copies"))
	if err := e.store.RecordConnectionTest(ctx, e.owner, c.ID, &interfaces.AzureTestResult{OK: true, LatencyMs: 5}); err != nil {
		t.Fatal(err)
	}
	got, _ := e.store.GetConnection(ctx, e.owner, c.ID)
	got.LastTest.OK = false
	got.Name = "mutated"
	again, _ := e.store.GetConnection(ctx, e.owner, c.ID)
	if again.LastTest == nil || !again.LastTest.OK || again.Name != "copies" {
		t.Errorf("mutating a returned record changed the store: %+v (LastTest %+v)", again, again.LastTest)
	}
	list, _ := e.store.ListConnections(ctx, e.owner)
	list[0].LastTest.LatencyMs = 999
	if again, _ = e.store.GetConnection(ctx, e.owner, c.ID); again.LastTest.LatencyMs != 5 {
		t.Errorf("mutating a listed record changed the store: %+v", again.LastTest)
	}
}

func azureState(t *testing.T, e azureEnv) {
	if st := e.store.State(context.Background()); st.State != interfaces.AzureRegistryOK || st.QuarantinePath != "" {
		t.Errorf("State of a fresh registry = %+v, want ok", st)
	}
}
