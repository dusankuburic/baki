package di

import (
	"context"
	"errors"
	"testing"

	"pad-analyzer/internal/config"
	"pad-analyzer/internal/service"
	"pad-analyzer/internal/testutil"
	"pad-analyzer/internal/testutil/redisreplica"
)

// REVIEW_COMPAT H1: a single replica that has PAD_REDIS_URL (rate limits, rooms, resume) keeps the in-process budget
// ledger. The Redis ledger fails closed, so giving it to a deployment with nothing to share would stop AI whenever
// Redis blinks. Only PAD_MAX_REPLICAS > 1 puts the ledger in Redis.
func TestProvideAIBudget_TheRedisLedgerIsForSeveralReplicasOnly(t *testing.T) {
	for _, tc := range []struct {
		name     string
		replicas int
		wantDown error // what a budget check says with Redis killed
	}{
		{"one replica with Redis", 1, nil},
		{"replicas unset with Redis", 0, nil},
		{"two replicas with Redis", 2, service.ErrAIBudgetUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := redisreplica.New(t, 1)
			r := c.Replicas[0]
			cfg := config.Default()
			cfg.Storage.MaxReplicas = tc.replicas
			budget := provideAIBudget(budgetSettings{budget: 5}, testutil.NewFakeBackend(), r.Client, r.Health, cfg)
			if err := budget.CheckBudget(context.Background(), "u1", "org-1"); err != nil {
				t.Fatalf("check with Redis up: %v", err)
			}
			c.Kill()
			err := budget.CheckBudget(context.Background(), "u1", "org-1")
			switch {
			case tc.wantDown == nil && err != nil:
				t.Errorf("check with Redis down = %v, want it to pass: the in-process ledger needs no Redis", err)
			case tc.wantDown != nil && !errors.Is(err, tc.wantDown):
				t.Errorf("check with Redis down = %v, want %v", err, tc.wantDown)
			}
		})
	}
}
