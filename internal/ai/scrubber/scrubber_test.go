package scrubber

import (
	"pad-analyzer/internal/models"
	"testing"
)

func TestScrubText(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "No secrets",
			input:    "This is a normal sentence.",
			expected: "This is a normal sentence.",
		},
		{
			name:     "Bearer token",
			input:    "Authorization: Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9...",
			expected: "Authorization: [REDACTED]",
		},
		{
			name:     "Stripe Secret Key",
			input:    "My key is sk_test_4eC39HqLyjWDarjtT1zdp7dc inside.",
			expected: "My key is [REDACTED] inside.",
		},
		{
			name:     "Generic Password",
			input:    "Config: password = super_secret_123;",
			expected: "Config: password = [REDACTED];",
		},
		{
			name:     "Connection String",
			input:    "Server=myServerAddress;Database=myDataBase;User Id=myUsername;Password=myPassword123;",
			expected: "Server=myServerAddress;Database=myDataBase;User Id=myUsername;Password=[REDACTED];",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ScrubText(tt.input)
			if got != tt.expected {
				t.Errorf("ScrubText() = %v, want %v", got, tt.expected)
			}
		})
	}
}

func TestScrubDocument(t *testing.T) {
	doc := &models.FlowDocument{
		ID: "flow-123",
		Subflows: []models.Subflow{
			{
				ID: "sf-1",
				Blocks: []models.Block{
					{
						ID:      "b-1",
						Type:    "ACTION",
						RawType: "WebAutomation.PopulateTextField",
						Properties: map[string]string{
							"Text": "my_secret_password",
							"Element": "Input_Field",
						},
					},
					{
						ID:      "b-2",
						Type:    "ACTION",
						RawType: "Database.Connect",
						Properties: map[string]string{
							"ConnectionString": "Server=localhost;Password=admin;",
						},
					},
				},
			},
		},
	}

	scrubbed, err := ScrubDocument(doc)
	if err != nil {
		t.Fatalf("ScrubDocument failed: %v", err)
	}

	if scrubbed == doc {
		t.Fatal("ScrubDocument should return a deep copy, got original pointer")
	}

	b1 := scrubbed.Subflows[0].Blocks[0]
	if b1.Properties["Text"] != "[REDACTED]" {
		t.Errorf("Expected WebAutomation.PopulateTextField Text to be redacted, got %s", b1.Properties["Text"])
	}
	if b1.Properties["Element"] != "Input_Field" {
		t.Errorf("Expected Element to be untouched, got %s", b1.Properties["Element"])
	}

	b2 := scrubbed.Subflows[0].Blocks[1]
	if b2.Properties["ConnectionString"] != "[REDACTED]" {
		t.Errorf("Expected Database.Connect ConnectionString to be redacted, got %s", b2.Properties["ConnectionString"])
	}
}

// TestScrubDocument_HighEntropyFallback verifies that an opaque high-entropy
// secret in an unknown action/field — which matches no known pattern — is still
// masked by the entropy fallback, while human-readable text is left intact.
func TestScrubDocument_HighEntropyFallback(t *testing.T) {
	doc := &models.FlowDocument{
		ID: "flow-1",
		Subflows: []models.Subflow{
			{
				ID: "sf-1",
				Blocks: []models.Block{
					{
						ID:      "b-1",
						Type:    "ACTION",
						RawType: "SomeVendor.CustomAction", // not in sensitiveActions
						Properties: map[string]string{
							// 40-char opaque token, no recognizable prefix or key=value form
							"CustomHeader": "a8F3kZ9pQ2rL7mW4xY1nB6tV0cD5eH8jK3gS2uP",
							"Description":  "Sends the daily summary report to the team",
						},
					},
				},
			},
		},
	}

	scrubbed, err := ScrubDocument(doc)
	if err != nil {
		t.Fatalf("ScrubDocument failed: %v", err)
	}

	b := scrubbed.Subflows[0].Blocks[0]
	if b.Properties["CustomHeader"] != "[REDACTED]" {
		t.Errorf("expected high-entropy CustomHeader to be redacted, got %q", b.Properties["CustomHeader"])
	}
	if b.Properties["Description"] != "Sends the daily summary report to the team" {
		t.Errorf("expected human-readable Description to be untouched, got %q", b.Properties["Description"])
	}
}
