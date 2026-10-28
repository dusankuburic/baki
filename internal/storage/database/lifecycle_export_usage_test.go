package database

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"testing"
	"time"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/storage/interfaces"
)

// TestPostgres_Lifecycle_ExportCarriesAIUsage: the data-subject export carries the user's AI usage rows (R-security-11),
// personal and organisation-billed (with the organisation's id), oldest first, and no other user's; rows that share a
// timestamp are neither dropped nor repeated by the keyset walk.
func TestPostgres_Lifecycle_ExportCarriesAIUsage(t *testing.T) {
	b, _ := openIsolatedMigrationDB(t)
	ctx := context.Background()
	f := lifecycleFixture{t: t, b: b, ctx: ctx}
	run := fmt.Sprintf("%x", time.Now().UnixNano())

	u, v := "usage-export-u-"+run, "usage-export-v-"+run
	for _, id := range []string{u, v} {
		f.ok("SaveUser "+id, b.SaveUser(ctx, &interfaces.User{ID: id, Email: id + "@example.com", Password: "x"}))
	}
	org := "usage-export-org-" + run
	f.ok("SaveOrg", b.SaveOrg(ctx, &interfaces.Organisation{ID: org, Name: "Usage", OwnerID: v,
		Members: []interfaces.OrgMember{{UserID: v, Role: auth.RoleAdmin}, {UserID: u, Role: auth.RoleMember}}}))

	base := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	tied := base.Add(time.Minute)
	mine := []interfaces.UsageMetric{
		{ID: "a-own-" + run, UserID: u, Provider: "openai", Model: "gpt", PromptTokens: 3, CompletionTokens: 1, CreatedAt: base},
		{ID: "b-org-" + run, UserID: u, OrgID: org, Provider: "anthropic", Model: "claude", ServedModel: "claude-x", PromptTokens: 10, CompletionTokens: 5, CreatedAt: tied},
		{ID: "c-tie-" + run, UserID: u, Provider: "openai", Model: "gpt", PromptTokens: 4, CompletionTokens: 2, CreatedAt: tied},
		{ID: "d-tie-" + run, UserID: u, Provider: "openai", Model: "gpt", PromptTokens: 5, CompletionTokens: 2, CreatedAt: tied},
		{ID: "e-late-" + run, UserID: u, Provider: "gemini", Model: "g", PromptTokens: 6, CompletionTokens: 3, CreatedAt: base.Add(2 * time.Minute)},
	}
	// Saved out of order, so the export's order is its own.
	for _, i := range []int{3, 0, 4, 1, 2} {
		m := mine[i]
		f.ok("SaveUsageMetric "+m.ID, b.SaveUsageMetric(ctx, &m))
	}
	f.ok("SaveUsageMetric other", b.SaveUsageMetric(ctx, &interfaces.UsageMetric{ID: "other-" + run, UserID: v, OrgID: org,
		Provider: "openai", Model: "gpt", PromptTokens: 7, CompletionTokens: 2, CreatedAt: tied}))

	var streamed bytes.Buffer
	f.ok("ExportUserDataTo", b.ExportUserDataTo(ctx, u, &streamed))
	var bundle map[string]json.RawMessage
	f.ok("unmarshal bundle", json.Unmarshal(streamed.Bytes(), &bundle))
	raw, ok := bundle["aiUsage"]
	if !ok {
		t.Fatalf("the export has no aiUsage section; keys: %v", keysOf(bundle))
	}
	var got []interfaces.UsageMetric
	f.ok("unmarshal aiUsage", json.Unmarshal(raw, &got))
	if !sameUsage(got, mine) {
		t.Errorf("exported aiUsage =\n %+v\nwant the user's rows oldest first (ties by id), the organisation-billed one with its org id:\n %+v", got, mine)
	}

	buffered, err := b.ExportUserData(ctx, u)
	f.ok("ExportUserData", err)
	if !sameUsage(buffered.AIUsage, mine) {
		t.Errorf("buffered AIUsage = %+v, want %+v", buffered.AIUsage, mine)
	}

	// A walk in pages smaller than the run of tied timestamps returns every row once.
	paged, err := b.exportAIUsage(ctx, u, 2)
	f.ok("exportAIUsage", err)
	if !sameUsage(paged, mine) {
		t.Errorf("paged walk = %+v, want %+v", paged, mine)
	}

	// A user without usage exports [] rather than null.
	empty := "usage-export-empty-" + run
	f.ok("SaveUser empty", b.SaveUser(ctx, &interfaces.User{ID: empty, Email: empty + "@example.com", Password: "x"}))
	var none bytes.Buffer
	f.ok("ExportUserDataTo empty", b.ExportUserDataTo(ctx, empty, &none))
	var emptyBundle map[string]json.RawMessage
	f.ok("unmarshal empty bundle", json.Unmarshal(none.Bytes(), &emptyBundle))
	if s := string(emptyBundle["aiUsage"]); s != "[]" {
		t.Errorf("aiUsage of a user without usage = %s, want []", s)
	}
}

func keysOf(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// sameUsage compares usage rows field by field, the timestamps as instants.
func sameUsage(got, want []interfaces.UsageMetric) bool {
	return slices.EqualFunc(got, want, func(a, b interfaces.UsageMetric) bool {
		at, bt := a.CreatedAt, b.CreatedAt
		a.CreatedAt, b.CreatedAt = time.Time{}, time.Time{}
		return a == b && at.Equal(bt)
	})
}
