package service

import (
	"context"
	"errors"
	"testing"

	"pad-core/models"
)

// policyStoreStub is an org-keyed in-memory PolicyStore (the fake backend's policy methods are no-ops).
type policyStoreStub struct {
	byKey map[string]*models.Policy
	err   error
}

func (s *policyStoreStub) SavePolicy(_ context.Context, p *models.Policy) error {
	if s.err != nil {
		return s.err
	}
	cp := *p
	s.byKey[p.OrgID+"/"+p.ID] = &cp
	return nil
}

func (s *policyStoreStub) RekeyIdlessPolicy(_ context.Context, orgID, id string) (bool, error) {
	p, ok := s.byKey[orgID+"/"]
	if !ok {
		return false, s.err
	}
	delete(s.byKey, orgID+"/")
	p.ID = id
	s.byKey[orgID+"/"+id] = p
	return true, nil
}

func (s *policyStoreStub) GetPolicy(_ context.Context, orgID, id string) (*models.Policy, error) {
	if p, ok := s.byKey[orgID+"/"+id]; ok {
		return p, nil
	}
	return nil, errors.New("not found")
}

func (s *policyStoreStub) ListPolicies(_ context.Context, orgID string) ([]*models.Policy, error) {
	var out []*models.Policy
	for _, p := range s.byKey {
		if p.OrgID == orgID {
			out = append(out, p)
		}
	}
	return out, nil
}

func (s *policyStoreStub) DeletePolicy(_ context.Context, orgID, id string) error {
	delete(s.byKey, orgID+"/"+id)
	return nil
}

func TestPolicyService_OrgScopedCRUD(t *testing.T) {
	store := &policyStoreStub{byKey: map[string]*models.Policy{}}
	svc := NewPolicyService(store)
	ctx := context.Background()

	if err := svc.Save(ctx, &models.Policy{ID: "p1", OrgID: "org-a", Name: "gate"}); err != nil {
		t.Fatal(err)
	}
	if err := svc.Save(ctx, &models.Policy{ID: "p2", OrgID: "org-b", Name: "other"}); err != nil {
		t.Fatal(err)
	}
	if got, err := svc.List(ctx, "org-a"); err != nil || len(got) != 1 || got[0].ID != "p1" {
		t.Fatalf("List(org-a) = %v, %v; want only p1", got, err)
	}
	if got, err := svc.Get(ctx, "org-a", "p1"); err != nil || got.Name != "gate" {
		t.Fatalf("Get = %v, %v", got, err)
	}
	if _, err := svc.Get(ctx, "org-a", "p2"); err == nil {
		t.Error("Get of another org's policy succeeded")
	}
	if err := svc.Delete(ctx, "org-a", "p1"); err != nil {
		t.Fatal(err)
	}
	if got, _ := svc.List(ctx, "org-a"); len(got) != 0 {
		t.Errorf("after Delete List = %v", got)
	}

	store.err = errors.New("db down")
	if err := svc.Save(ctx, &models.Policy{ID: "p3", OrgID: "org-a"}); !errors.Is(err, store.err) {
		t.Errorf("Save error = %v, want the store's", err)
	}
}

// TestPolicyService_SaveGivesAnIdlessPolicyAnID: a policy saved without an id gets a generated one, written back
// onto it, so two id-less policies of one organisation no longer overwrite each other; an id the caller gave is kept.
func TestPolicyService_SaveGivesAnIdlessPolicyAnID(t *testing.T) {
	store := &policyStoreStub{byKey: map[string]*models.Policy{}}
	svc := NewPolicyService(store)
	ctx := context.Background()

	a, b := &models.Policy{OrgID: "org-a", Name: "a"}, &models.Policy{OrgID: "org-a", Name: "b"}
	for _, p := range []*models.Policy{a, b} {
		if err := svc.Save(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	if a.ID == "" || b.ID == "" || a.ID == b.ID {
		t.Fatalf("ids = %q, %q; want two distinct generated ids", a.ID, b.ID)
	}
	if got, _ := svc.List(ctx, "org-a"); len(got) != 2 {
		t.Errorf("List = %d policies, want both", len(got))
	}
	named := &models.Policy{ID: "mine", OrgID: "org-a", Name: "c"}
	if err := svc.Save(ctx, named); err != nil || named.ID != "mine" {
		t.Errorf("an explicit id: %q, %v; want it kept", named.ID, err)
	}
}
