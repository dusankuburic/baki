package service

import (
	"context"
	"errors"
	"math"
	"strings"
	"sync"
	"testing"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/config"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
	"pad-core/models"
)

// TestValidateDeploymentAISettings pins the 400 contract of PUT /api/admin/settings/ai: what the budget gate or
// the knowledge indexer could not use is refused with the field named, and a registry-embedded provider (its
// models are deployment aliases) only on the operator's PAD_EMBEDDING_PROVIDER pin, which the write cannot set.
func TestValidateDeploymentAISettings(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   DeploymentAISettings
		want string // in the error, "" = accepted
	}{
		{"zero is unlimited", DeploymentAISettings{}, ""},
		{"budgets and a provider that embeds", DeploymentAISettings{DailyBudget: 50, PerUserDailyBudget: 5, EmbeddingProvider: "openai", EmbeddingModel: "text-embedding-3-small"}, ""},
		{"a model id at the cap", DeploymentAISettings{EmbeddingModel: strings.Repeat("m", maxEmbeddingModelChars)}, ""},
		{"negative daily budget", DeploymentAISettings{DailyBudget: -1}, "ai.dailyBudget"},
		{"negative per-user budget", DeploymentAISettings{PerUserDailyBudget: -0.01}, "ai.perUserDailyBudget"},
		{"NaN", DeploymentAISettings{DailyBudget: math.NaN()}, "ai.dailyBudget"},
		{"infinite", DeploymentAISettings{PerUserDailyBudget: math.Inf(1)}, "ai.perUserDailyBudget"},
		{"a provider no spec describes", DeploymentAISettings{EmbeddingProvider: "nope"}, "ai.embeddingProvider"},
		{"a provider that cannot embed", DeploymentAISettings{EmbeddingProvider: "claude"}, "ai.embeddingProvider"},
		{"a registry-embedded provider", DeploymentAISettings{EmbeddingProvider: "azure-foundry"}, "PAD_EMBEDDING_PROVIDER=azure-foundry"},
		{"an oversized model id", DeploymentAISettings{EmbeddingModel: strings.Repeat("m", maxEmbeddingModelChars+1)}, "ai.embeddingModel"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateDeploymentAISettings(tc.in)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				return
			}
			if !errors.Is(err, ErrSettingsInvalid) {
				t.Fatalf("err = %v, want ErrSettingsInvalid", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %q, want it to name %q", err, tc.want)
			}
		})
	}
}

// pendingWriteBackend keeps a settings write invisible to reads until commit, as a row written inside the request's
// transaction is to a read on another connection (what the provider's cache refresh uses) until the request ends.
type pendingWriteBackend struct {
	testutil.FakeBackend
	mu      sync.Mutex
	pending *storageif.AppSettings
}

func (b *pendingWriteBackend) SaveSettings(_ context.Context, s *storageif.AppSettings) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pending = s
	return nil
}

func (b *pendingWriteBackend) commit(t *testing.T) {
	t.Helper()
	b.mu.Lock()
	pending := b.pending
	b.pending = nil
	b.mu.Unlock()
	if pending == nil {
		t.Fatal("nothing was written")
	}
	if err := b.FakeBackend.SaveSettings(context.Background(), pending); err != nil {
		t.Fatal(err)
	}
}

// TestUpdateDeploymentAISettings_AnswersWithTheWrite: the handler answered a PUT with a re-read through the
// provider's cache, which refreshes outside the request's transaction and so saw the row as it was before the
// write; the write itself is the answer. The rest of the document is kept.
func TestUpdateDeploymentAISettings_AnswersWithTheWrite(t *testing.T) {
	ctx := context.Background()
	b := &pendingWriteBackend{}
	seed := models.DefaultSettings()
	seed.AI.DailyBudget = 50
	seed.Appearance.Theme = "light"
	if err := b.FakeBackend.SaveSettings(ctx, settingsFromModel(seed)); err != nil {
		t.Fatal(err)
	}
	sys := NewSystemService(NewBackendSettingsProvider(ctx, b), nil, &testutil.CountingNotifier{}, b, config.ModeCloud, nil, nil)

	in := DeploymentAISettings{DailyBudget: 200, PerUserDailyBudget: 20, EmbeddingProvider: "gemini", EmbeddingModel: "text-embedding-004"}
	got, err := sys.UpdateDeploymentAISettings(ctx, in)
	if err != nil {
		t.Fatalf("UpdateDeploymentAISettings: %v", err)
	}
	if got != in {
		t.Errorf("UpdateDeploymentAISettings answered %+v, want the written %+v", got, in)
	}
	// The re-read the handler used to answer with: the row as the other connection still sees it.
	if reread, _ := sys.GetDeploymentAISettings(ctx); reread.DailyBudget != 50 {
		t.Fatalf("the test double's write is visible before commit (%+v); it no longer models the transaction", reread)
	}

	b.commit(t)
	stored, err := b.LoadSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stored.AI.DailyBudget != 200 || stored.AI.EmbeddingProvider != "gemini" || stored.Appearance.Theme != "light" {
		t.Errorf("stored ai = %+v, theme %q; want the written values with the rest of the document kept", stored.AI, stored.Appearance.Theme)
	}
}

// TestUpdateDeploymentAISettings_RefusesWhatTheIndexerCannotUse: a refused write is ErrSettingsInvalid (the API's
// 400) and writes nothing.
func TestUpdateDeploymentAISettings_RefusesWhatTheIndexerCannotUse(t *testing.T) {
	ctx := context.Background()
	b := &countingBackend{}
	sys := NewSystemService(NewBackendSettingsProvider(ctx, b), nil, &testutil.CountingNotifier{}, b, config.ModeCloud, nil, nil)
	_, err := sys.UpdateDeploymentAISettings(ctx, DeploymentAISettings{EmbeddingProvider: "azure-foundry"})
	if !errors.Is(err, ErrSettingsInvalid) {
		t.Fatalf("err = %v, want ErrSettingsInvalid", err)
	}
	if n := b.saves.Load(); n != 0 {
		t.Errorf("%d settings writes after a refused update, want none", n)
	}
}

// TestContextTokenBudget_ClampsToTheClientMaximum: the caller's own document sets their per-provider context
// budget, so a settings write past the Settings UI's maximum (an API client; the form stops at it) would have
// every turn build a context of that size. The server clamps to the same maximum.
func TestContextTokenBudget_ClampsToTheClientMaximum(t *testing.T) {
	b := &countingBackend{FakeBackend: testutil.FakeBackend{Users: map[string]*storageif.User{"u1": {ID: "u1"}}}}
	svc := &ChatService{mode: config.ModeCloud, backend: b, settings: NewBackendSettingsProvider(context.Background(), b)}
	ctx := auth.WithClaims(context.Background(), &auth.Claims{UserID: "u1"})
	withBudget := func(n int) {
		s := models.DefaultSettings()
		s.AI.Providers = map[string]models.AIProviderConfig{"openai": {ContextTokenBudget: n}}
		if err := b.SaveUserSettings(context.Background(), "u1", settingsFromModel(s)); err != nil {
			t.Fatal(err)
		}
	}
	withBudget(5_000_000)
	if got := svc.contextTokenBudget(ctx, "openai"); got != MaxContextTokenBudget {
		t.Errorf("contextTokenBudget = %d for a 5 000 000-token document, want the client's maximum %d", got, MaxContextTokenBudget)
	}
	withBudget(8000)
	if got := svc.contextTokenBudget(ctx, "openai"); got != 8000 {
		t.Errorf("contextTokenBudget = %d for an 8000-token document, want 8000", got)
	}
}
