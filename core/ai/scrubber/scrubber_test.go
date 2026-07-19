package scrubber

import (
	"pad-core/models"
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
		{
			name:     "Connection String with Pwd alias",
			input:    "Server=db;Uid=sa;Pwd=hunter2secret;",
			expected: "Server=db;Uid=sa;Pwd=[REDACTED];",
		},
		{
			name:     "YAML-style key with spaced colon",
			input:    "api_key : AKIA1234567890ABCDEF",
			expected: "api_key : [REDACTED]",
		},

		// H15: AWS access key IDs (AKIA = normal, ASIA = temporary STS).
		// Word-bounded so "AKIA1234567890ABCDEF" (16 chars) is masked but a
		// shorter prefix like "AKIATEST" stays visible.
		{
			name:     "AWS access key AKIA",
			input:    "Use role AKIAIOSFODNN7EXAMPLE to deploy.",
			expected: "Use role [REDACTED] to deploy.",
		},
		{
			name:     "AWS STS token ASIA",
			input:    "export AWS_ACCESS_KEY_ID=ASIA3RF3M2X7HEXAMPLE",
			expected: "export AWS_ACCESS_KEY_ID=[REDACTED]",
		},
		// H15: Google API key (AIza + exactly 35 chars = 39 chars total).
		{
			name:     "Google API key",
			input:    "maps_key = AIzaSyxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx",
			expected: "maps_key = [REDACTED]",
		},
		// H15: Slack token (xox[abprs]- prefix).
		{
			name:     "Slack bot token",
			input:    "SLACK_BOT_TOKEN=xoxb-123456789012-abcdefghij",
			expected: "SLACK_BOT_TOKEN=[REDACTED]",
		},
		// H15: JWT (eyJ prefix on header + payload).
		{
			name:     "JWT in bearer header",
			input:    "Authorization: Bearer eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJV_adQssw5c",
			expected: "Authorization: [REDACTED]",
		},
		// H15: PEM private key block (multi-line).
		{
			name: "PEM RSA private key",
			input: `Here is the key:
-----BEGIN RSA PRIVATE KEY-----
MIIEpAIBAAKCAQEAz7v5g3qP6mJZ...
-----END RSA PRIVATE KEY-----
Don't leak it.`,
			expected: `Here is the key:
[REDACTED]
Don't leak it.`,
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
							"Text":    "my_secret_password",
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

	// The original document must be left untouched (deep-copy contract). This
	// guards the typed clone, which — unlike the prior JSON round-trip — could
	// alias the caller's Properties maps if cloneBlocks regressed.
	if got := doc.Subflows[0].Blocks[0].Properties["Text"]; got != "my_secret_password" {
		t.Errorf("original document was mutated: Text = %q, want unchanged", got)
	}
	if got := doc.Subflows[0].Blocks[1].Properties["ConnectionString"]; got != "Server=localhost;Password=admin;" {
		t.Errorf("original document was mutated: ConnectionString = %q, want unchanged", got)
	}
}

// TestScrubDocument_NestedChildrenDeepCopied verifies that secrets in nested
// child blocks are masked in the copy while the original child stays untouched —
// exercising the recursive cloneBlocks path.
func TestScrubDocument_NestedChildrenDeepCopied(t *testing.T) {
	doc := &models.FlowDocument{
		ID: "flow-nested",
		Subflows: []models.Subflow{{
			ID: "sf-1",
			Blocks: []models.Block{{
				ID:   "loop-1",
				Type: "LOOP",
				Children: []models.Block{{
					ID:      "child-1",
					Type:    "ACTION",
					RawType: "Database.Connect",
					Properties: map[string]string{
						"ConnectionString": "Server=db;Password=hunter2;",
					},
				}},
			}},
		}},
	}

	scrubbed, err := ScrubDocument(doc)
	if err != nil {
		t.Fatalf("ScrubDocument failed: %v", err)
	}

	if got := scrubbed.Subflows[0].Blocks[0].Children[0].Properties["ConnectionString"]; got != "[REDACTED]" {
		t.Errorf("nested child secret not redacted in copy: %q", got)
	}
	if got := doc.Subflows[0].Blocks[0].Children[0].Properties["ConnectionString"]; got != "Server=db;Password=hunter2;" {
		t.Errorf("original nested child was mutated: %q", got)
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

// TestScrubDocument_SensitiveFieldNames verifies that a property is masked when
// its KEY names a credential, even on an action not listed in sensitiveActions
// and even when the value is short/low-entropy (so the regex and entropy passes
// would otherwise miss it).
func TestScrubDocument_SensitiveFieldNames(t *testing.T) {
	doc := &models.FlowDocument{
		ID: "flow-1",
		Subflows: []models.Subflow{
			{
				ID: "sf-1",
				Blocks: []models.Block{
					{
						ID:      "b-1",
						Type:    "ACTION",
						RawType: "CustomConnector.Invoke", // not enumerated
						Properties: map[string]string{
							"Password":    "pw1", // short, low entropy
							"Api_Key":     "abc", // separator + short
							"AccessToken": "xyz",
							"Endpoint":    "https://api.example.com/v1",
							"RequestName": "Get daily report",
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
	for _, k := range []string{"Password", "Api_Key", "AccessToken"} {
		if b.Properties[k] != "[REDACTED]" {
			t.Errorf("expected credential field %q to be redacted, got %q", k, b.Properties[k])
		}
	}
	if b.Properties["Endpoint"] != "https://api.example.com/v1" {
		t.Errorf("expected Endpoint URL untouched, got %q", b.Properties["Endpoint"])
	}
	if b.Properties["RequestName"] != "Get daily report" {
		t.Errorf("expected RequestName untouched, got %q", b.Properties["RequestName"])
	}
}

func TestNormalizeFieldName(t *testing.T) {
	cases := map[string]string{
		"Api_Key": "apikey", "api-key": "apikey", "ApiKey": "apikey",
		"Connection String": "connectionstring", "PWD": "pwd",
	}
	for in, want := range cases {
		if got := normalizeFieldName(in); got != want {
			t.Errorf("normalizeFieldName(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestLooksLikeHighEntropySecret_LengthTieredAndPathGuard pins the S3 tuning:
// the entropy threshold is lowered for long opaque strings (so a 64-char hex
// token entropying ~3.7 is caught, where the old flat >4.0 bar missed it), while
// filesystem paths — which also clear the entropy bar — are excluded so the AI
// keeps them for analysing file/folder actions. Entropies were measured with
// shannonEntropy; see the S3 note in docs/IMPROVEMENTS.md.
func TestLooksLikeHighEntropySecret_LengthTieredAndPathGuard(t *testing.T) {
	// Long opaque hex token (64 chars, entropy ~3.67): previously missed by the
	// flat >4.0 threshold, now caught by the len>50 ⇒ >3.5 tier.
	hexToken := "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	if !looksLikeHighEntropySecret(hexToken) {
		t.Errorf("expected long low-entropy hex token (len>50) to be flagged, got false (entropy=%.2f)", shannonEntropy(hexToken))
	}

	// Filesystem paths: long, space-free, entropy >4.0, but NOT secrets. The
	// path guard must keep them regardless of the entropy tier.
	for _, path := range []string{
		`C:\Users\john.doe\Documents\Projects\Reports\Quarterly`, // Windows, entropy ~4.24
		"/home/john.doe/projects/reports/quarterly/2024/final",   // POSIX, entropy ~4.25
	} {
		if looksLikeHighEntropySecret(path) {
			t.Errorf("expected filesystem path to be preserved (path guard), got flagged: %q", path)
		}
	}

	// Short opaque token (len ≤ 50) still needs the strict >4.0 bar; a short
	// low-entropy identifier must not be masked.
	shortIdent := "server-prod-us-east-1-primary" // 29 chars, entropy < 4.0, no spaces
	if looksLikeHighEntropySecret(shortIdent) {
		t.Errorf("expected short low-entropy identifier to be left intact, got flagged: %q", shortIdent)
	}

	// Sanity: the existing high-entropy 39-char token stays flagged.
	if !looksLikeHighEntropySecret("a8F3kZ9pQ2rL7mW4xY1nB6tV0cD5eH8jK3gS2uP") {
		t.Error("expected the canonical 39-char high-entropy token to be flagged")
	}
}
