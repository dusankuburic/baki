package interfaces

import (
	"context"
	"time"
)

// UsageStore covers AI usage/cost tracking for budget enforcement.
type UsageStore interface {
	SaveUsageMetric(ctx context.Context, metric *UsageMetric) error
	GetDailyUsage(ctx context.Context, userID, orgID string) (float64, error)
}

// UsageMetric is one AI call's usage row: Model is the id the caller asked for; ServedModel is the model the provider reported answering when different (alias/router/agent), empty otherwise.
type UsageMetric struct {
	ID               string    `json:"id"`
	UserID           string    `json:"userId"`
	OrgID            string    `json:"orgId"`
	Provider         string    `json:"provider"`
	Model            string    `json:"model"`
	ServedModel      string    `json:"servedModel,omitempty"`
	PromptTokens     int       `json:"promptTokens"`
	CompletionTokens int       `json:"completionTokens"`
	EstimatedCost    float64   `json:"estimatedCost"`
	CreatedAt        time.Time `json:"createdAt"`
}
