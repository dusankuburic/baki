package service

import (
	"context"
	"errors"
	"strings"
	"testing"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/rag"
	"pad-analyzer/internal/storage"
)

func newAzmKBEnv(t *testing.T) *azmEnv {
	return newAzureModelsEnv(t, func(d *AzureModelServiceDeps) { d.KnowledgeEmbeddingEnabled = true })
}

// Every change to the corpus's embedding model on a non-empty corpus needs
// a confirmed re-index (409 REINDEX_REQUIRED, then force): designating
// another model, editing the designated one's deployment or resource,
// changing its resource's endpoint, deleting either. Other edits do not.
func TestAzureModels_ReindexRequired(t *testing.T) {
	e := newAzmKBEnv(t)
	ctx := context.Background()
	org := AzureScope{Scope: "admin", OrgID: "org-1"}
	c := e.mustConn(t, org, "c", azmKeyConfig("k"))
	// An empty corpus: nothing to re-index.
	kb := e.mustModel(t, org, AzureModelInput{ConnectionID: c.ID, Kind: "embedding", Deployment: "emb1", IsKnowledgeEmbedding: true})
	other := e.mustModel(t, org, AzureModelInput{ConnectionID: c.ID, Kind: "embedding", Deployment: "emb2"})
	chat := e.mustModel(t, org, AzureModelInput{ConnectionID: c.ID, Kind: "chat", Deployment: "gpt-4o"})
	e.corpus.set("org-1", 12)

	isReindex := func(name string, err error) {
		t.Helper()
		if azmCode(err) != AzureCodeReindexRequired {
			t.Errorf("%s: %v, want REINDEX_REQUIRED", name, err)
		}
	}
	yes := true
	_, err := e.svc.CreateModel(ctx, org, AzureModelInput{ConnectionID: c.ID, Kind: "embedding", Deployment: "emb3", IsKnowledgeEmbedding: true})
	isReindex("create a new knowledge-base model", err)
	_, err = e.svc.UpdateModel(ctx, org, other.ID, AzureModelPatch{IsKnowledgeEmbedding: &yes})
	isReindex("designate another model", err)
	dep := "emb1-v2"
	_, err = e.svc.UpdateModel(ctx, org, kb.ID, AzureModelPatch{Deployment: &dep})
	isReindex("edit the designated deployment", err)
	moved := azmKeyConfig("k2")
	moved.Endpoint = "https://my-res.openai.azure.com"
	_, err = e.svc.UpdateConnection(ctx, org, c.ID, AzureConnectionPatch{Config: &moved})
	isReindex("change its resource's endpoint", err)
	err = e.svc.DeleteModel(ctx, org, kb.ID, false)
	isReindex("delete the designated model", err)
	_, err = e.svc.DeleteConnection(ctx, org, c.ID, AzureDeleteOptions{Cascade: true})
	isReindex("delete the resource holding it", err)

	// Not corpus changes: another model's edit, a case-only rename of the
	// same deployment, a new key on the same endpoint.
	name := "GPT"
	if _, err := e.svc.UpdateModel(ctx, org, chat.ID, AzureModelPatch{DisplayName: &name}); err != nil {
		t.Errorf("unrelated edit: %v", err)
	}
	same := "EMB1"
	if _, err := e.svc.UpdateModel(ctx, org, kb.ID, AzureModelPatch{Deployment: &same}); err != nil {
		t.Errorf("case-only rename: %v", err)
	}
	rekey := azmKeyConfig("k3")
	cur, _ := e.svc.GetConnection(ctx, org, c.ID)
	if _, err := e.svc.UpdateConnection(ctx, org, c.ID, AzureConnectionPatch{Config: &rekey, CredentialUpdatedAt: cur.CredentialUpdatedAt}); err != nil {
		t.Errorf("new key, same endpoint: %v", err)
	}
	// Confirmed.
	if _, err := e.svc.UpdateModel(ctx, org, other.ID, AzureModelPatch{IsKnowledgeEmbedding: &yes, Force: true}); err != nil {
		t.Fatalf("forced designation: %v", err)
	}
	if m, _ := e.svc.GetModel(ctx, org, kb.ID); m.IsKnowledgeEmbedding {
		t.Error("the designation did not move")
	}
	res, err := e.svc.DeleteConnection(ctx, org, c.ID, AzureDeleteOptions{Cascade: true, Force: true})
	if err != nil || !res.ClearedKnowledgeEmbedding || res.DeletedModels != 3 {
		t.Errorf("forced delete = %+v, %v", res, err)
	}
	// Clearing the designation is no re-index.
	c2 := e.mustConn(t, org, "c2", azmKeyConfig("k"))
	kb2 := e.mustModel(t, org, AzureModelInput{ConnectionID: c2.ID, Kind: "embedding", Deployment: "e", IsKnowledgeEmbedding: true, Force: true})
	no := false
	if _, err := e.svc.UpdateModel(ctx, org, kb2.ID, AzureModelPatch{IsKnowledgeEmbedding: &no}); err != nil {
		t.Errorf("clearing: %v", err)
	}
}

// A user's own designation embeds the corpus of every organisation they
// administer that has no resources of its own: those corpora count.
func TestAzureModels_ReindexRequiredPersonal(t *testing.T) {
	e := newAzmKBEnv(t)
	ctx := context.Background()
	e.orgs.add("own-kb", "Own", map[string]auth.Role{"u1": auth.RoleAdmin})
	e.orgs.add("org-kb", "WithResources", map[string]auth.Role{"u1": auth.RoleAdmin})
	e.mustConn(t, AzureScope{Scope: "u1", OrgID: "org-kb"}, "theirs", azmKeyConfig("k"))
	c := e.mustConn(t, azmUser, "c", azmKeyConfig("k"))
	e.corpus.set("org-kb", 50) // embedded with the org's own model
	if _, err := e.svc.CreateModel(ctx, azmUser, AzureModelInput{ConnectionID: c.ID, Kind: "embedding", Deployment: "e1", IsKnowledgeEmbedding: true}); err != nil {
		t.Fatalf("an org with resources of its own does not count: %v", err)
	}
	e.corpus.set("own-kb", 3)
	if _, err := e.svc.CreateModel(ctx, azmUser, AzureModelInput{ConnectionID: c.ID, Kind: "embedding", Deployment: "e2", IsKnowledgeEmbedding: true}); azmCode(err) != AzureCodeReindexRequired {
		t.Errorf("the corpus the personal model embeds: %v", err)
	}
}

// The knowledge base's embedder: the organisation's designation whenever it
// holds resources — none designated is a refusal, never a member's personal
// model — else the caller's own.
func TestAzureModels_KnowledgeEmbedder(t *testing.T) {
	e := newAzmKBEnv(t)
	ctx := context.Background()
	org := AzureScope{Scope: "admin", OrgID: "org-1"}
	mine := e.mustConn(t, azmUser, "mine", azmKeyConfig("k"))
	e.mustModel(t, azmUser, AzureModelInput{ConnectionID: mine.ID, Kind: "embedding", Deployment: "my-emb", IsKnowledgeEmbedding: true})

	// No org resources: the member's own.
	p, err := e.svc.KnowledgeEmbedder(ctx, "u1", "org-1")
	if err != nil || ai.UsedOrgCredential(p) {
		t.Fatalf("personal embedder = %v, %v", p, err)
	}
	// The org has resources but no designation: refused, never personal.
	oc := e.mustConn(t, org, "org", azmKeyConfig("ok"))
	_, err = e.svc.KnowledgeEmbedder(ctx, "u1", "org-1")
	if !errors.Is(err, ai.ErrOrgEmbeddingDeploymentMissing) {
		t.Fatalf("org without a designation: %v", err)
	}
	// Designated: the org's credential.
	e.mustModel(t, org, AzureModelInput{ConnectionID: oc.ID, Kind: "embedding", Deployment: "org-emb", IsKnowledgeEmbedding: true})
	p, err = e.svc.KnowledgeEmbedder(ctx, "u1", "org-1")
	if err != nil || !ai.UsedOrgCredential(p) {
		t.Fatalf("org embedder = %v, %v", p, err)
	}
	// Its credential gone: still the org's problem, no fallback.
	_ = e.orgKeys.DeleteOrg("org-1", "azure-foundry/"+oc.ID)
	if _, err := e.svc.KnowledgeEmbedder(ctx, "u1", "org-1"); !errors.Is(err, ai.ErrOrgEmbeddingDeploymentMissing) || !errors.Is(err, ai.ErrKeyNotConfigured) {
		t.Errorf("org credential gone: %v", err)
	}
	// Nothing designated for the caller: the plain sentinel (the knowledge
	// base may look further).
	_, err = e.svc.KnowledgeEmbedder(ctx, "u2", "")
	if !errors.Is(err, ai.ErrEmbeddingDeploymentMissing) || errors.Is(err, ai.ErrOrgEmbeddingDeploymentMissing) {
		t.Errorf("no designation: %v", err)
	}
	if l, _ := e.svc.ListConnections(ctx, azmUser); !l.KnowledgeEmbeddingEnabled {
		t.Error("knowledgeEmbeddingEnabled not listed")
	}
}

// The designation is offered only where the knowledge base embeds with
// Azure; clearing one (an imported designation) stays possible.
func TestAzureModels_KnowledgeDesignationGated(t *testing.T) {
	e := newAzureModelsEnv(t)
	ctx := context.Background()
	c := e.mustConn(t, azmUser, "c", azmKeyConfig("k"))
	m := e.mustModel(t, azmUser, AzureModelInput{ConnectionID: c.ID, Kind: "embedding", Deployment: "e"})
	yes, no := true, false
	if _, err := e.svc.UpdateModel(ctx, azmUser, m.ID, AzureModelPatch{IsKnowledgeEmbedding: &yes}); azmCode(err) != AzureCodeInvalidDesignation {
		t.Errorf("designation where it is not offered: %v", err)
	}
	if _, err := e.svc.UpdateModel(ctx, azmUser, m.ID, AzureModelPatch{IsKnowledgeEmbedding: &no}); err != nil {
		t.Errorf("clearing: %v", err)
	}
	if l, _ := e.svc.ListConnections(ctx, azmUser); l.KnowledgeEmbeddingEnabled {
		t.Error("knowledgeEmbeddingEnabled listed where it is not offered")
	}
}

// S6: a registry or key-store failure while the knowledge base's embedder
// is resolved is logged and answered with the transient
// rag.ErrEmbeddingSetupUnavailable, whose copy the client may see — never the
// driver's text (hosts, roles, database names), never a configuration
// refusal, never a fallback to another model. Real configuration gaps keep
// their refusals.
func TestAzureModels_KnowledgeEmbedderMasksStoreFailures(t *testing.T) {
	ctx := context.Background()
	const driverText = "failed to connect to `host=10.1.2.3 user=baki_app database=baki`: dial error (dial tcp 10.1.2.3:5432: connect: connection refused)"
	e := newAzmKBEnv(t)
	e.orgs.add("o1", "Org", map[string]auth.Role{"a1": auth.RoleAdmin})
	masked := func(what string, err error) {
		t.Helper()
		if !errors.Is(err, rag.ErrEmbeddingSetupUnavailable) || errors.Is(err, ai.ErrEmbeddingDeploymentMissing) ||
			errors.Is(err, ai.ErrOrgEmbeddingDeploymentMissing) || errors.Is(err, ai.ErrKeyNotConfigured) {
			t.Errorf("%s: %v, want only the transient refusal", what, err)
		}
		if err != nil && (strings.Contains(err.Error(), "10.1.2.3") || strings.Contains(err.Error(), "baki_app")) {
			t.Errorf("%s leaks the driver's text: %v", what, err)
		}
	}
	e.reg.FailOn("ListConnections", errors.New(driverText))
	_, err := e.svc.KnowledgeEmbedder(ctx, "a1", "o1")
	masked("the org registry unreadable", err)
	// Through the knowledge base: no EMBEDDING_NOT_CONFIGURED (400 with the
	// message) — the transient code, the copy only.
	ks := rag.NewKnowledgeService(nil, e.factory, nil, rag.WithEmbeddingProvider("azure-foundry"), rag.WithAzureEmbedder(e.svc))
	err = ks.AddDocument(ctx, "a1", "o1", "f.md", "hello")
	if errors.Is(err, rag.ErrEmbeddingUnavailable) {
		t.Errorf("AddDocument: %v — a store failure is no configuration gap", err)
	}
	masked("AddDocument", err)
	e.reg.ClearFailures()

	// A key-store failure building the designated model's provider.
	c := e.mustConn(t, azmUser, "mine", azmKeyConfig("k"))
	e.mustModel(t, azmUser, AzureModelInput{ConnectionID: c.ID, Kind: "embedding", Deployment: "emb", IsKnowledgeEmbedding: true})
	key := ai.AzureConnectionKey(c.ID)
	lookupErr := error(errors.New(driverText))
	f := ai.NewProviderFactory(func(scope, provider string) (string, error) {
		if provider == key && lookupErr != nil {
			return "", lookupErr
		}
		v, _ := e.secrets.Get(scope, provider)
		return v, nil
	}, nil, nil, nil)
	e.svc.factory = f
	_, err = e.svc.KnowledgeEmbedder(ctx, "u1", "")
	masked("the key store failing", err)

	// A credential the server can no longer decrypt is the user's to fix: a
	// configuration refusal that says so, not the transient one.
	lookupErr = storage.ErrSecretDecryptFailed
	_, err = e.svc.KnowledgeEmbedder(ctx, "u1", "")
	if !errors.Is(err, ai.ErrProviderConfigInvalid) || errors.Is(err, rag.ErrEmbeddingSetupUnavailable) || !strings.Contains(err.Error(), "enter them again") {
		t.Errorf("an undecryptable credential: %v", err)
	}
	// No designation at all: the plain sentinel (the knowledge base looks
	// further), as before.
	if _, err := e.svc.KnowledgeEmbedder(ctx, "u9", ""); !errors.Is(err, ai.ErrEmbeddingDeploymentMissing) {
		t.Errorf("nothing designated: %v", err)
	}
}
