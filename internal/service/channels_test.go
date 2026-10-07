package service

import (
	"context"
	"testing"

	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
)

func TestChannelService_OrgScoped(t *testing.T) {
	b := testutil.NewFakeBackend()
	svc := NewChannelService(b)
	ctx := context.Background()

	for _, ch := range []*storageif.OrgChannel{
		{ID: "c1", OrgID: "org-a", Name: "ops", Kind: "webhook", URL: "https://hooks.example/1"},
		{ID: "c2", OrgID: "org-a", Name: "off", Kind: "slack", URL: "https://hooks.example/2"},
		{ID: "c3", OrgID: "org-b", Name: "theirs", Kind: "teams", URL: "https://hooks.example/3"},
	} {
		if err := svc.Save(ctx, ch); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := svc.List(ctx, "org-a"); err != nil || len(got) != 2 {
		t.Fatalf("List(org-a) = %v, %v; want 2", got, err)
	}

	if got, err := svc.Find(ctx, "org-a", "c2"); err != nil || got == nil || got.Name != "off" {
		t.Fatalf("Find(org-a, c2) = %v, %v", got, err)
	}
	if got, err := svc.Find(ctx, "org-a", "c3"); err != nil || got != nil {
		t.Errorf("Find of another org's channel = %v, %v; want nil, nil", got, err)
	}

	if err := svc.Delete(ctx, "org-a", "c1"); err != nil {
		t.Fatal(err)
	}
	if got, _ := svc.Find(ctx, "org-a", "c1"); got != nil {
		t.Error("Delete left the channel")
	}
}
